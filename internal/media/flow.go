package media

import (
	"slices"
	"sync"
	"time"

	"bork/internal/identity"
)

const maxReceivedFramesPerSource = 2

type AudioStreamKind byte

const (
	AudioStreamVoice AudioStreamKind = iota
	AudioStreamScreen
)

type ReceivedFrame struct {
	SourceID   identity.PeerID
	StreamKind AudioStreamKind
	StreamID   [16]byte
	Sequence   uint64
	Timestamp  uint32
	Payload    []byte
	ReceivedAt time.Time
}

type SendFrame struct {
	Timestamp      uint32
	Payload        []byte
	Deadline       time.Time
	SendGeneration uint64
}

type PeerPort interface {
	SendReady() <-chan struct{}
	ConsumeSend(func(SendFrame)) bool
	SubmitReceived(ReceivedFrame) bool
	SetSendInvalidator(func(uint64))
}

type AudioPort interface {
	ReceivedReady() <-chan struct{}
	TakeReceived() (ReceivedFrame, bool)
	ScreenAudioSource() identity.PeerID
	SubmitSend(SendFrame) bool
	InvalidateSend() uint64
	Reset() uint64
}

// Flow is a room-scoped ownership boundary between one Audio producer/consumer
// and one Peer producer/consumer. Accepted payload ownership transfers to Flow.
type Flow struct {
	mu sync.Mutex

	receivedQueues      map[receivedStreamKey][]ReceivedFrame
	receivedOrder       []receivedStreamKey
	receivedNext        int
	receivedReady       chan struct{}
	screenAudioSourceID identity.PeerID

	send            SendFrame
	hasSend         bool
	sendGeneration  uint64
	sendInvalidator func(uint64)
	sendReady       chan struct{}
}

// receivedStreamKey keeps a peer's voice and screen audio in separate queues.
// Each stream has its own packet sequence and decoder state.
type receivedStreamKey struct {
	sourceID identity.PeerID
	kind     AudioStreamKind
}

func NewFlow() *Flow {
	return &Flow{
		receivedQueues: make(map[receivedStreamKey][]ReceivedFrame),
		receivedReady:  make(chan struct{}, 1),
		sendGeneration: 1,
		sendReady:      make(chan struct{}, 1),
	}
}

func (f *Flow) ReceivedReady() <-chan struct{} { return f.receivedReady }
func (f *Flow) SendReady() <-chan struct{}     { return f.sendReady }

func (f *Flow) SubmitReceived(frame ReceivedFrame) bool {
	if frame.SourceID.IsZero() || frame.Sequence == 0 || len(frame.Payload) == 0 {
		return false
	}
	f.mu.Lock()
	if frame.StreamKind == AudioStreamScreen && frame.SourceID != f.screenAudioSourceID {
		f.mu.Unlock()
		return false
	}
	key := receivedStreamKey{sourceID: frame.SourceID, kind: frame.StreamKind}
	queue := f.receivedQueues[key]
	if len(queue) == 0 {
		f.receivedOrder = append(f.receivedOrder, key)
	}
	if len(queue) > 0 && queue[0].StreamID != frame.StreamID {
		queue = nil
	}
	insertAt := len(queue)
	for index := range queue {
		if queue[index].Sequence == frame.Sequence {
			f.mu.Unlock()
			return false
		}
		if queue[index].Sequence > frame.Sequence {
			insertAt = index
			break
		}
	}
	queue = append(queue, ReceivedFrame{})
	copy(queue[insertAt+1:], queue[insertAt:])
	queue[insertAt] = frame
	if len(queue) > maxReceivedFramesPerSource {
		queue = queue[len(queue)-maxReceivedFramesPerSource:]
	}
	f.receivedQueues[key] = queue
	notify(f.receivedReady)
	f.mu.Unlock()
	return true
}

func (f *Flow) TakeReceived() (ReceivedFrame, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for offset := range len(f.receivedOrder) {
		index := (f.receivedNext + offset) % len(f.receivedOrder)
		key := f.receivedOrder[index]
		queue := f.receivedQueues[key]
		if len(queue) == 0 {
			continue
		}
		frame := queue[0]
		if len(queue) == 1 {
			delete(f.receivedQueues, key)
			f.receivedOrder = append(f.receivedOrder[:index], f.receivedOrder[index+1:]...)
			if len(f.receivedOrder) == 0 {
				f.receivedNext = 0
			} else {
				f.receivedNext = index % len(f.receivedOrder)
			}
		} else {
			f.receivedQueues[key] = queue[1:]
			f.receivedNext = (index + 1) % len(f.receivedOrder)
		}
		return frame, true
	}
	return ReceivedFrame{}, false
}

// SetScreenAudioSource makes screen audio follow the screen selected by the
// viewer. An empty source means the local preview or no remote screen.
func (f *Flow) SetScreenAudioSource(sourceID identity.PeerID) {
	f.mu.Lock()
	if f.screenAudioSourceID == sourceID {
		f.mu.Unlock()
		return
	}
	oldKey := receivedStreamKey{sourceID: f.screenAudioSourceID, kind: AudioStreamScreen}
	delete(f.receivedQueues, oldKey)
	f.receivedOrder = slices.DeleteFunc(f.receivedOrder, func(key receivedStreamKey) bool { return key == oldKey })
	f.screenAudioSourceID = sourceID
	f.receivedNext = 0
	notify(f.receivedReady)
	f.mu.Unlock()
}

func (f *Flow) ScreenAudioSource() identity.PeerID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.screenAudioSourceID
}

func (f *Flow) SubmitSend(frame SendFrame) bool {
	if len(frame.Payload) == 0 {
		return false
	}
	f.mu.Lock()
	if frame.SendGeneration != f.sendGeneration {
		f.mu.Unlock()
		return false
	}
	f.send = frame
	f.hasSend = true
	notify(f.sendReady)
	f.mu.Unlock()
	return true
}

// ConsumeSend keeps the send lease valid until consume returns. This lets
// InvalidateSend synchronously wait for an in-flight fan-out.
func (f *Flow) ConsumeSend(consume func(SendFrame)) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.hasSend {
		return false
	}
	frame := f.send
	f.send = SendFrame{}
	f.hasSend = false
	consume(frame)
	return true
}

func (f *Flow) InvalidateSend() uint64 {
	f.mu.Lock()
	f.advanceSendGenerationLocked()
	f.send = SendFrame{}
	f.hasSend = false
	drain(f.sendReady)
	generation := f.sendGeneration
	if f.sendInvalidator != nil {
		f.sendInvalidator(generation)
	}
	f.mu.Unlock()
	return generation
}

func (f *Flow) Reset() uint64 {
	f.mu.Lock()
	f.advanceSendGenerationLocked()
	clear(f.receivedQueues)
	f.receivedOrder = f.receivedOrder[:0]
	f.receivedNext = 0
	f.send = SendFrame{}
	f.hasSend = false
	drain(f.receivedReady)
	drain(f.sendReady)
	generation := f.sendGeneration
	if f.sendInvalidator != nil {
		f.sendInvalidator(generation)
	}
	f.mu.Unlock()
	return generation
}

func (f *Flow) SetSendInvalidator(invalidator func(uint64)) {
	f.mu.Lock()
	f.sendInvalidator = invalidator
	if invalidator != nil {
		invalidator(f.sendGeneration)
	}
	f.mu.Unlock()
}

func (f *Flow) advanceSendGenerationLocked() {
	f.sendGeneration++
	if f.sendGeneration == 0 {
		f.sendGeneration = 1
	}
}

func notify(channel chan<- struct{}) {
	select {
	case channel <- struct{}{}:
	default:
	}
}

func drain(channel <-chan struct{}) {
	select {
	case <-channel:
	default:
	}
}

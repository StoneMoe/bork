package audio

import "sync/atomic"

type pcmFrame struct {
	Samples        [FrameSamples]float32
	Reference      [FrameSamples]float32
	Timestamp      uint32
	Index          uint64
	Generation     uint64
	LocalOnly      bool
	Muted          bool
	ReferenceValid bool
}

// pcmFrameQueue is a strict single-producer/single-consumer queue. A slot is
// owned by the producer until CommitWrite and by the consumer until ReleaseRead.
type pcmFrameQueue struct {
	frames []pcmFrame
	read   atomic.Uint64
	write  atomic.Uint64
}

func newPCMFrameQueue(capacity int) *pcmFrameQueue {
	return &pcmFrameQueue{frames: make([]pcmFrame, capacity)}
}

func (q *pcmFrameQueue) AcquireWrite() (*pcmFrame, bool) {
	read := q.read.Load()
	write := q.write.Load()
	if write-read >= uint64(len(q.frames)) {
		return nil, false
	}
	return &q.frames[write%uint64(len(q.frames))], true
}

func (q *pcmFrameQueue) CommitWrite() {
	q.write.Add(1)
}

func (q *pcmFrameQueue) AcquireRead() (*pcmFrame, bool) {
	read := q.read.Load()
	if read == q.write.Load() {
		return nil, false
	}
	return &q.frames[read%uint64(len(q.frames))], true
}

func (q *pcmFrameQueue) ReleaseRead() {
	q.read.Add(1)
}

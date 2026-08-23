package audio

import (
	"errors"
	"fmt"
	"math"
	"slices"

	"bork/internal/media"

	"github.com/thesyncim/gopus"
)

type jitterFrame struct {
	sequence uint64
	payload  []byte
}

type jitterStream struct {
	decoder           *gopus.Decoder
	streamID          [16]byte
	frames            map[uint32]jitterFrame
	expectedTimestamp uint32
	latestTimestamp   uint32
	latestSequence    uint64
	sequenceFloor     uint64
	hasLatest         bool
	started           bool
	idle              int
	losses            int
	pcm               []float32
	speaking          speakingHold
	normalizer        loudnessNormalizer
}

type mixerStreamKey struct {
	peerID string
	kind   media.AudioStreamKind
}

type mixer struct {
	streams               map[mixerStreamKey]*jitterStream
	maxFrameBytes         int
	speakingPeerIDs       []string
	loudnessNormalization bool
	screenAudioSourceID   string
}

func newMixer(maxFrameBytes int) *mixer {
	return &mixer{
		streams:               make(map[mixerStreamKey]*jitterStream),
		maxFrameBytes:         maxFrameBytes,
		speakingPeerIDs:       []string{},
		loudnessNormalization: true,
	}
}

func (m *mixer) Add(frame media.ReceivedFrame) error {
	key := mixerStreamKey{peerID: frame.SourceID, kind: frame.StreamKind}
	// Selection can change after Flow removes a frame from its queue but before
	// it reaches the mixer. Recheck here so a fast A-B-A switch cannot add B.
	if key.kind == media.AudioStreamScreen && key.peerID != m.screenAudioSourceID {
		return nil
	}
	stream := m.streams[key]
	if stream == nil {
		decoder, err := newOpusDecoder(m.maxFrameBytes)
		if err != nil {
			return err
		}
		stream = &jitterStream{
			decoder:  decoder,
			streamID: frame.StreamID,
			frames:   make(map[uint32]jitterFrame),
			pcm:      make([]float32, FrameSamples),
		}
		stream.normalizer.reset()
		m.streams[key] = stream
	}
	if frame.StreamID != stream.streamID {
		resetJitterStream(stream, frame.StreamID, 0)
	}
	if frame.Sequence == 0 || len(frame.Payload) == 0 {
		return nil
	}
	if frame.Sequence < stream.sequenceFloor {
		return nil
	}
	if stream.hasLatest && frame.Sequence > stream.latestSequence {
		delta := int32(frame.Timestamp - stream.latestTimestamp)
		if delta <= 0 || delta%FrameSamples != 0 || delta > int32(maxJitterFrames*FrameSamples) {
			resetJitterStream(stream, frame.StreamID, frame.Sequence)
		}
	}
	if stream.started {
		ahead := int32(frame.Timestamp - stream.expectedTimestamp)
		if ahead < 0 {
			return nil
		}
		if ahead%FrameSamples != 0 || ahead > int32(maxJitterFrames*FrameSamples) {
			resetJitterStream(stream, frame.StreamID, frame.Sequence)
		}
	}
	if _, exists := stream.frames[frame.Timestamp]; exists {
		return nil
	}
	if len(stream.frames) >= maxJitterFrames {
		farthestTimestamp := frame.Timestamp
		farthestDistance := int32(-1)
		base := stream.expectedTimestamp
		if !stream.started && stream.hasLatest {
			base = stream.latestTimestamp
		}
		for timestamp := range stream.frames {
			distance := int32(timestamp - base)
			if distance > farthestDistance {
				farthestTimestamp = timestamp
				farthestDistance = distance
			}
		}
		newDistance := int32(frame.Timestamp - base)
		if newDistance >= farthestDistance {
			return nil
		}
		delete(stream.frames, farthestTimestamp)
	}
	stream.frames[frame.Timestamp] = jitterFrame{sequence: frame.Sequence, payload: frame.Payload}
	if !stream.hasLatest || frame.Sequence > stream.latestSequence {
		stream.latestSequence = frame.Sequence
		stream.latestTimestamp = frame.Timestamp
		stream.hasLatest = true
	}
	stream.idle = 0
	return nil
}

func (m *mixer) setScreenAudioSource(sourceID string) {
	if m.screenAudioSourceID == sourceID {
		return
	}
	delete(m.streams, mixerStreamKey{peerID: m.screenAudioSourceID, kind: media.AudioStreamScreen})
	m.screenAudioSourceID = sourceID
}

func resetJitterStream(stream *jitterStream, streamID [16]byte, sequenceFloor uint64) {
	stream.decoder.Reset()
	clear(stream.frames)
	stream.streamID = streamID
	stream.expectedTimestamp = 0
	stream.latestTimestamp = 0
	stream.latestSequence = 0
	stream.sequenceFloor = sequenceFloor
	stream.hasLatest = false
	stream.started = false
	stream.losses = 0
	stream.normalizer.reset()
}

func (m *mixer) NextInto(destination []float32) (bool, error) {
	if len(destination) != FrameSamples {
		return false, errors.New("mix destination has an invalid frame size")
	}
	clear(destination)
	active := 0
	var mixErr error
	for key, stream := range m.streams {
		stream.idle++
		if stream.idle > streamIdleFrames {
			delete(m.streams, key)
			continue
		}
		if !stream.started {
			start, ok := contiguousStart(stream.frames)
			if !ok {
				if key.kind == media.AudioStreamVoice {
					stream.speaking.update(false)
					stream.normalizer.process(nil, 0, false, m.loudnessNormalization)
				}
				continue
			}
			stream.expectedTimestamp = start
			stream.started = true
			stream.losses = 0
		}

		frame, exists := stream.frames[stream.expectedTimestamp]
		if exists {
			delete(stream.frames, stream.expectedTimestamp)
			stream.losses = 0
		} else {
			stream.losses++
			if stream.losses > maxPLCFrames {
				stream.decoder.Reset()
				stream.started = false
				stream.losses = 0
				if key.kind == media.AudioStreamVoice {
					stream.speaking.update(false)
					stream.normalizer.reset()
				}
				continue
			}
		}
		var count int
		var err error
		if !exists {
			if next, hasNext := stream.frames[stream.expectedTimestamp+FrameSamples]; hasNext {
				count, err = stream.decoder.DecodeWithFEC(next.payload, stream.pcm, true)
			} else {
				count, err = stream.decoder.Decode(nil, stream.pcm)
			}
		} else {
			count, err = stream.decoder.Decode(frame.payload, stream.pcm)
		}
		if err != nil {
			delete(m.streams, key)
			if mixErr == nil {
				mixErr = fmt.Errorf("decode audio from %s: %w", key.peerID, err)
			}
			continue
		}
		if count != FrameSamples {
			delete(m.streams, key)
			if mixErr == nil {
				mixErr = errors.New("Opus decoder returned an unexpected frame size")
			}
			continue
		}
		stream.expectedTimestamp += FrameSamples
		level := pcmRMS(stream.pcm)
		// Screen audio keeps the source application's dynamics and must not make
		// the sharing peer appear to be speaking.
		if key.kind == media.AudioStreamVoice {
			stream.speaking.update(level > speakingThreshold)
			stream.normalizer.process(stream.pcm, level, exists, m.loudnessNormalization)
		}
		if !(level > 0.0001) {
			continue
		}
		active++
		for index := range destination {
			destination[index] += stream.pcm[index]
		}
	}
	m.refreshSpeakingPeerIDs()
	if active == 0 {
		return false, mixErr
	}
	for index, sample := range destination {
		if active > 1 {
			sample /= float32(active)
		}
		destination[index] = max(-1, min(1, sample))
	}
	return true, mixErr
}

func (m *mixer) SpeakingPeerIDs() []string {
	return m.speakingPeerIDs
}

func (m *mixer) refreshSpeakingPeerIDs() {
	m.speakingPeerIDs = m.speakingPeerIDs[:0]
	for key, stream := range m.streams {
		if key.kind == media.AudioStreamVoice && stream.speaking.active() {
			m.speakingPeerIDs = append(m.speakingPeerIDs, key.peerID)
		}
	}
	slices.Sort(m.speakingPeerIDs)
}

func contiguousStart(frames map[uint32]jitterFrame) (uint32, bool) {
	if len(frames) < prebufferFrames {
		return 0, false
	}
	var selected uint32
	var selectedSequence uint64
	found := false
	for timestamp, frame := range frames {
		contiguous := true
		for offset := 1; offset < prebufferFrames; offset++ {
			if _, exists := frames[timestamp+uint32(offset*FrameSamples)]; !exists {
				contiguous = false
				break
			}
		}
		if contiguous && (!found || frame.sequence < selectedSequence) {
			selected = timestamp
			selectedSequence = frame.sequence
			found = true
		}
	}
	return selected, found
}

func pcmRMS(pcm []float32) float64 {
	var energy float64
	for _, sample := range pcm {
		energy += float64(sample * sample)
	}
	return math.Sqrt(energy / float64(len(pcm)))
}

type loudnessNormalizer struct {
	measuredRMS float64
	gain        float64
	holdFrames  int
}

func (n *loudnessNormalizer) reset() {
	*n = loudnessNormalizer{gain: 1}
}

func (n *loudnessNormalizer) process(pcm []float32, rawRMS float64, measured, enabled bool) {
	if n.gain == 0 {
		n.gain = 1
	}
	if measured && rawRMS > speakingThreshold {
		if n.measuredRMS == 0 {
			n.measuredRMS = rawRMS
		} else {
			coefficient := 1.0 / float64(normalizationReleaseFrames)
			if rawRMS > n.measuredRMS {
				coefficient = 1.0 / float64(normalizationAttackFrames)
			}
			n.measuredRMS += (rawRMS - n.measuredRMS) * coefficient
		}
		n.holdFrames = normalizationHoldFrames
	} else if n.holdFrames > 0 {
		n.holdFrames--
	}

	target := 1.0
	if enabled && n.holdFrames > 0 && n.measuredRMS > 0 {
		target = max(normalizationMinimumGain, min(normalizationMaximumGain, normalizationTargetRMS/n.measuredRMS))
	}
	nextGain := n.gain + (target-n.gain)/float64(normalizationSmoothingFrames)
	applyGainRamp(pcm, float32(n.gain), float32(nextGain))
	n.gain = nextGain
}

type speakingHold struct {
	frames int
}

func (h *speakingHold) update(loud bool) bool {
	wasActive := h.active()
	if loud {
		h.frames = speakingReleaseFrames
	} else if h.frames > 0 {
		h.frames--
	}
	return wasActive != h.active()
}

func (h *speakingHold) active() bool {
	return h.frames > 0
}

func (h *speakingHold) reset() {
	h.frames = 0
}

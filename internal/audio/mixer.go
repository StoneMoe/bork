package audio

import (
	"errors"
	"fmt"
	"math"

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
}

type mixer struct {
	streams       map[string]*jitterStream
	maxFrameBytes int
}

func newMixer(maxFrameBytes int) *mixer {
	return &mixer{streams: make(map[string]*jitterStream), maxFrameBytes: maxFrameBytes}
}

func (m *mixer) Add(frame media.ReceivedFrame) error {
	stream := m.streams[frame.SourceID]
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
		m.streams[frame.SourceID] = stream
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
}

func (m *mixer) NextInto(destination []float32) (bool, error) {
	if len(destination) != FrameSamples {
		return false, errors.New("mix destination has an invalid frame size")
	}
	clear(destination)
	active := 0
	var mixErr error
	for peerID, stream := range m.streams {
		stream.idle++
		if stream.idle > streamIdleFrames {
			delete(m.streams, peerID)
			continue
		}
		if !stream.started {
			start, ok := contiguousStart(stream.frames)
			if !ok {
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
			delete(m.streams, peerID)
			if mixErr == nil {
				mixErr = fmt.Errorf("decode voice from %s: %w", peerID, err)
			}
			continue
		}
		if count != FrameSamples {
			delete(m.streams, peerID)
			if mixErr == nil {
				mixErr = errors.New("Opus decoder returned an unexpected frame size")
			}
			continue
		}
		stream.expectedTimestamp += FrameSamples
		if !audible(stream.pcm) {
			continue
		}
		active++
		for index := range destination {
			destination[index] += stream.pcm[index]
		}
	}
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

func audible(pcm []float32) bool {
	var energy float64
	for _, sample := range pcm {
		energy += float64(sample * sample)
	}
	return math.Sqrt(energy/float64(len(pcm))) > 0.0001
}

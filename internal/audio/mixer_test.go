package audio

import (
	"math"
	"testing"

	"bork/internal/media"
)

const testMaxEncodedFrameBytes = 1200

func TestMixerPrebuffersAndDecodesOutOfOrderFrames(t *testing.T) {
	encoder, err := newOpusEncoder(testMaxEncodedFrameBytes)
	if err != nil {
		t.Fatal(err)
	}
	packets := make(map[uint64][]byte)
	for sequence := uint64(1); sequence <= prebufferFrames; sequence++ {
		pcm := make([]float32, FrameSamples)
		for index := range pcm {
			pcm[index] = float32(math.Sin(float64(index)*0.03)) * 0.2
		}
		packets[sequence], err = encoder.Encode(pcm)
		if err != nil {
			t.Fatal(err)
		}
	}
	mixer := newMixer(testMaxEncodedFrameBytes)
	if err := mixer.Add(media.ReceivedFrame{SourceID: "peer", Sequence: 2, Timestamp: 2 * FrameSamples, Payload: packets[2]}); err != nil {
		t.Fatal(err)
	}
	if _, active, err := mixerNext(mixer); err != nil || active {
		t.Fatalf("mixer started before prebuffer: active=%v err=%v", active, err)
	}
	for _, sequence := range []uint64{1, 3} {
		if err := mixer.Add(media.ReceivedFrame{SourceID: "peer", Sequence: sequence, Timestamp: uint32(sequence) * FrameSamples, Payload: packets[sequence]}); err != nil {
			t.Fatal(err)
		}
	}
	pcm, active, err := mixerNext(mixer)
	if err != nil || !active {
		t.Fatalf("mixer did not start: active=%v err=%v", active, err)
	}
	nonzero := false
	for _, sample := range pcm {
		if sample != 0 {
			nonzero = true
			break
		}
	}
	if !nonzero {
		t.Fatal("decoded PCM is silent")
	}
}

func TestMixerUsesPLCForMissingFrame(t *testing.T) {
	encoder, err := newOpusEncoder(testMaxEncodedFrameBytes)
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]float32, FrameSamples)
	for index := range pcm {
		pcm[index] = 0.1
	}
	packet, err := encoder.Encode(pcm)
	if err != nil {
		t.Fatal(err)
	}
	mixer := newMixer(testMaxEncodedFrameBytes)
	for _, sequence := range []uint64{1, 3, 4} {
		if err := mixer.Add(media.ReceivedFrame{SourceID: "peer", Sequence: sequence, Timestamp: uint32(sequence) * FrameSamples, Payload: packet}); err != nil {
			t.Fatal(err)
		}
	}
	if _, active, err := mixerNext(mixer); err != nil || !active {
		t.Fatalf("initial decode: active=%v err=%v", active, err)
	}
	if _, active, err := mixerNext(mixer); err != nil || !active {
		t.Fatalf("PLC decode: active=%v err=%v", active, err)
	}
}

func TestMixerResetsAfterLargeSequenceJump(t *testing.T) {
	packet := audiblePacket(t)
	mixer := newMixer(testMaxEncodedFrameBytes)
	for sequence := uint64(1); sequence <= prebufferFrames; sequence++ {
		if err := mixer.Add(media.ReceivedFrame{SourceID: "peer", Sequence: sequence, Timestamp: uint32(sequence) * FrameSamples, Payload: packet}); err != nil {
			t.Fatal(err)
		}
	}
	if _, active, err := mixerNext(mixer); err != nil || !active {
		t.Fatalf("initial decode: active=%v err=%v", active, err)
	}
	jump := uint64(maxJitterFrames + 100)
	if err := mixer.Add(media.ReceivedFrame{SourceID: "peer", Sequence: jump, Timestamp: uint32(jump) * FrameSamples, Payload: packet}); err != nil {
		t.Fatal(err)
	}
	if _, active, err := mixerNext(mixer); err != nil || active {
		t.Fatalf("mixer did not rebuffer after jump: active=%v err=%v", active, err)
	}
	if err := mixer.Add(media.ReceivedFrame{SourceID: "peer", Sequence: jump + 1, Timestamp: uint32(jump+1) * FrameSamples, Payload: packet}); err != nil {
		t.Fatal(err)
	}
	if _, active, err := mixerNext(mixer); err != nil || !active {
		t.Fatalf("decode after rebuffer: active=%v err=%v", active, err)
	}
}

func TestMixerRebuffersWhenSenderTimestampRestarts(t *testing.T) {
	packet := audiblePacket(t)
	mixer := newMixer(testMaxEncodedFrameBytes)
	for sequence := uint64(10); sequence < 10+prebufferFrames; sequence++ {
		if err := mixer.Add(media.ReceivedFrame{SourceID: "peer", Sequence: sequence, Timestamp: uint32(sequence) * FrameSamples, Payload: packet}); err != nil {
			t.Fatal(err)
		}
	}
	if _, active, err := mixerNext(mixer); err != nil || !active {
		t.Fatalf("initial decode: active=%v err=%v", active, err)
	}
	if err := mixer.Add(media.ReceivedFrame{SourceID: "peer", Sequence: 12, Timestamp: FrameSamples, Payload: packet}); err != nil {
		t.Fatal(err)
	}
	if _, active, err := mixerNext(mixer); err != nil || active {
		t.Fatalf("mixer did not rebuffer after timestamp restart: active=%v err=%v", active, err)
	}
	if err := mixer.Add(media.ReceivedFrame{SourceID: "peer", Sequence: 13, Timestamp: 2 * FrameSamples, Payload: packet}); err != nil {
		t.Fatal(err)
	}
	if _, active, err := mixerNext(mixer); err != nil || !active {
		t.Fatalf("decode after timestamp restart: active=%v err=%v", active, err)
	}
}

func TestMixerAcceptsSequenceRestartForNewSession(t *testing.T) {
	packet := audiblePacket(t)
	mixer := newMixer(testMaxEncodedFrameBytes)
	firstStream := [16]byte{1}
	secondStream := [16]byte{2}
	for sequence := uint64(10); sequence < 10+prebufferFrames; sequence++ {
		if err := mixer.Add(media.ReceivedFrame{SourceID: "peer", StreamID: firstStream, Sequence: sequence, Timestamp: uint32(sequence) * FrameSamples, Payload: packet}); err != nil {
			t.Fatal(err)
		}
	}
	if _, active, err := mixerNext(mixer); err != nil || !active {
		t.Fatalf("initial decode: active=%v err=%v", active, err)
	}
	for sequence := uint64(1); sequence <= prebufferFrames; sequence++ {
		if err := mixer.Add(media.ReceivedFrame{SourceID: "peer", StreamID: secondStream, Sequence: sequence, Timestamp: uint32(sequence) * FrameSamples, Payload: packet}); err != nil {
			t.Fatal(err)
		}
	}
	if _, active, err := mixerNext(mixer); err != nil || !active {
		t.Fatalf("decode after session restart: active=%v err=%v", active, err)
	}
}

func mixerNext(mixer *mixer) ([]float32, bool, error) {
	pcm := make([]float32, FrameSamples)
	active, err := mixer.NextInto(pcm)
	return pcm, active, err
}

func TestMixerTreatsTimestampGapAsLossInsteadOfRestart(t *testing.T) {
	packet := audiblePacket(t)
	mixer := newMixer(testMaxEncodedFrameBytes)
	for sequence := uint64(1); sequence <= 2; sequence++ {
		if err := mixer.Add(media.ReceivedFrame{SourceID: "peer", Sequence: sequence, Timestamp: uint32(sequence) * FrameSamples, Payload: packet}); err != nil {
			t.Fatal(err)
		}
	}
	if _, active, err := mixerNext(mixer); err != nil || !active {
		t.Fatalf("initial decode: active=%v err=%v", active, err)
	}
	if err := mixer.Add(media.ReceivedFrame{SourceID: "peer", Sequence: 3, Timestamp: 4 * FrameSamples, Payload: packet}); err != nil {
		t.Fatal(err)
	}
	stream := mixer.streams["peer"]
	if !stream.started || stream.expectedTimestamp != 2*FrameSamples {
		t.Fatalf("timestamp gap restarted stream: %#v", stream)
	}
}

func TestMixerHandlesTimestampWrap(t *testing.T) {
	packet := audiblePacket(t)
	mixer := newMixer(testMaxEncodedFrameBytes)
	start := ^uint32(0) - (FrameSamples - 1)
	if err := mixer.Add(media.ReceivedFrame{SourceID: "peer", Sequence: 1, Timestamp: start, Payload: packet}); err != nil {
		t.Fatal(err)
	}
	if err := mixer.Add(media.ReceivedFrame{SourceID: "peer", Sequence: 2, Timestamp: 0, Payload: packet}); err != nil {
		t.Fatal(err)
	}
	if _, active, err := mixerNext(mixer); err != nil || !active {
		t.Fatalf("decode across timestamp wrap: active=%v err=%v", active, err)
	}
}

func TestMixerRejectsOlderSequencesAfterTimestampRestart(t *testing.T) {
	packet := audiblePacket(t)
	mixer := newMixer(testMaxEncodedFrameBytes)
	for sequence := uint64(10); sequence <= 11; sequence++ {
		if err := mixer.Add(media.ReceivedFrame{SourceID: "peer", Sequence: sequence, Timestamp: uint32(sequence) * FrameSamples, Payload: packet}); err != nil {
			t.Fatal(err)
		}
	}
	if _, active, err := mixerNext(mixer); err != nil || !active {
		t.Fatalf("initial decode: active=%v err=%v", active, err)
	}
	if err := mixer.Add(media.ReceivedFrame{SourceID: "peer", Sequence: 12, Timestamp: FrameSamples, Payload: packet}); err != nil {
		t.Fatal(err)
	}
	if err := mixer.Add(media.ReceivedFrame{SourceID: "peer", Sequence: 9, Timestamp: 9 * FrameSamples, Payload: packet}); err != nil {
		t.Fatal(err)
	}
	stream := mixer.streams["peer"]
	if stream.sequenceFloor != 12 || len(stream.frames) != 1 {
		t.Fatalf("old sequence entered restarted jitter buffer: %#v", stream)
	}
}

func audiblePacket(t *testing.T) []byte {
	t.Helper()
	encoder, err := newOpusEncoder(testMaxEncodedFrameBytes)
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]float32, FrameSamples)
	for index := range pcm {
		pcm[index] = float32(math.Sin(float64(index)*0.03)) * 0.2
	}
	packet, err := encoder.Encode(pcm)
	if err != nil {
		t.Fatal(err)
	}
	return packet
}

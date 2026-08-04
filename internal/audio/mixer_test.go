package audio

import (
	"math"
	"slices"
	"testing"

	"bork/internal/media"
)

const testMaxEncodedFrameBytes = 1200

func TestSpeakingThresholdAndReleaseHold(t *testing.T) {
	pcm := make([]float32, FrameSamples)
	for index := range pcm {
		pcm[index] = 0.001
	}
	if level := pcmRMS(pcm); !(level > 0.0001) || level > speakingThreshold {
		t.Fatal("speaking threshold changed the lower mixer audibility threshold")
	}
	for index := range pcm {
		pcm[index] = 0.02
	}
	if pcmRMS(pcm) <= speakingThreshold {
		t.Fatal("loud PCM did not cross the speaking threshold")
	}

	var hold speakingHold
	if !hold.update(true) || !hold.active() {
		t.Fatal("loud frame did not activate speaking hold")
	}
	for tick := 1; tick < speakingReleaseFrames; tick++ {
		if hold.update(false) || !hold.active() {
			t.Fatalf("speaking hold released after %dms", tick*FrameDuration)
		}
	}
	if !hold.update(false) || hold.active() {
		t.Fatalf("speaking hold did not release after %dms", speakingReleaseFrames*FrameDuration)
	}
}

func TestMixerMapsSpeakingToSortedSourceIDs(t *testing.T) {
	loud := packetWithAmplitude(t, 0.2)
	quiet := packetWithAmplitude(t, 0.001)
	mixer := newMixer(testMaxEncodedFrameBytes)
	for _, source := range []struct {
		id     string
		packet []byte
	}{
		{"peer-z", loud},
		{"peer-quiet", quiet},
		{"peer-a", loud},
	} {
		for sequence := uint64(1); sequence <= prebufferFrames; sequence++ {
			if err := mixer.Add(media.ReceivedFrame{SourceID: source.id, Sequence: sequence, Timestamp: uint32(sequence) * FrameSamples, Payload: source.packet}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(mixer.SpeakingPeerIDs()) != 0 {
		t.Fatal("packet presence alone marked a source as speaking")
	}
	if _, _, err := mixerNext(mixer); err != nil {
		t.Fatal(err)
	}
	if got, want := mixer.SpeakingPeerIDs(), []string{"peer-a", "peer-z"}; !slices.Equal(got, want) {
		t.Fatalf("speaking peer IDs = %v, want %v", got, want)
	}

	for _, stream := range mixer.streams {
		stream.started = false
		clear(stream.frames)
	}
	for range speakingReleaseFrames - 1 {
		if _, _, err := mixerNext(mixer); err != nil {
			t.Fatal(err)
		}
	}
	if got := mixer.SpeakingPeerIDs(); len(got) != 2 {
		t.Fatalf("missing output released speaking early: %v", got)
	}
	if _, _, err := mixerNext(mixer); err != nil {
		t.Fatal(err)
	}
	if got := mixer.SpeakingPeerIDs(); len(got) != 0 {
		t.Fatalf("missing output did not release speaking: %v", got)
	}
}

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

func TestMixerUsesPLCForTimestampGap(t *testing.T) {
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
	if _, active, err := mixerNext(mixer); err != nil || !active {
		t.Fatalf("pre-gap decode: active=%v err=%v", active, err)
	}
	if err := mixer.Add(media.ReceivedFrame{SourceID: "peer", Sequence: 3, Timestamp: 4 * FrameSamples, Payload: packet}); err != nil {
		t.Fatal(err)
	}
	if _, active, err := mixerNext(mixer); err != nil || !active {
		t.Fatalf("PLC decode: active=%v err=%v", active, err)
	}
	if _, active, err := mixerNext(mixer); err != nil || !active {
		t.Fatalf("post-gap decode: active=%v err=%v", active, err)
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
	return packetWithAmplitude(t, 0.2)
}

func packetWithAmplitude(t *testing.T, amplitude float64) []byte {
	t.Helper()
	encoder, err := newOpusEncoder(testMaxEncodedFrameBytes)
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]float32, FrameSamples)
	for index := range pcm {
		pcm[index] = float32(math.Sin(float64(index)*0.03) * amplitude)
	}
	packet, err := encoder.Encode(pcm)
	if err != nil {
		t.Fatal(err)
	}
	return packet
}

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

func TestLoudnessNormalizerConvergesAndClamps(t *testing.T) {
	for _, test := range []struct {
		level float64
		gain  float64
	}{
		{0.5, normalizationMinimumGain},
		{0.05, 2},
		{0.02, normalizationMaximumGain},
	} {
		var normalizer loudnessNormalizer
		normalizer.reset()
		driveNormalizer(&normalizer, test.level, true, true, 300)
		if math.Abs(normalizer.gain-test.gain) > 0.001 {
			t.Fatalf("level %.2f converged to gain %.4f, want %.2f", test.level, normalizer.gain, test.gain)
		}
		if normalizer.gain < normalizationMinimumGain || normalizer.gain > normalizationMaximumGain {
			t.Fatalf("normalizer gain %.4f escaped clamp", normalizer.gain)
		}
	}
}

func TestLoudnessNormalizerHoldAndSmoothBypass(t *testing.T) {
	var normalizer loudnessNormalizer
	normalizer.reset()
	driveNormalizer(&normalizer, 0.2, true, true, 300)
	heldGain := normalizer.gain
	driveNormalizer(&normalizer, 0, false, true, normalizationHoldFrames-1)
	if math.Abs(normalizer.gain-heldGain) > 0.001 {
		t.Fatalf("gain changed during hold: %.4f to %.4f", heldGain, normalizer.gain)
	}
	driveNormalizer(&normalizer, 0, false, true, 1)
	if normalizer.gain <= heldGain {
		t.Fatal("gain did not release toward unity after hold")
	}

	normalizer.reset()
	driveNormalizer(&normalizer, 0.2, true, true, 300)
	measured := normalizer.measuredRMS
	normalizedGain := normalizer.gain
	driveNormalizer(&normalizer, 0.2, true, false, 1)
	if normalizer.gain <= normalizedGain || normalizer.gain >= 1 {
		t.Fatalf("bypass did not begin smoothly: %.4f to %.4f", normalizedGain, normalizer.gain)
	}
	driveNormalizer(&normalizer, 0.2, true, false, 300)
	if math.Abs(normalizer.gain-1) > 0.001 || normalizer.measuredRMS != measured {
		t.Fatalf("bypass gain/measurement = %.4f/%.4f, want 1/%.4f", normalizer.gain, normalizer.measuredRMS, measured)
	}
	driveNormalizer(&normalizer, 0, false, true, 1)
	if normalizer.gain >= 1 {
		t.Fatal("re-enabled normalizer did not use retained measurement")
	}
}

func TestMixerDoesNotTrainNormalizerOnFECOrPLC(t *testing.T) {
	quiet := packetWithAmplitude(t, 0.05)
	loud := packetWithAmplitude(t, 0.5)
	mixer := newMixer(testMaxEncodedFrameBytes)
	for sequence := uint64(1); sequence <= prebufferFrames; sequence++ {
		if err := mixer.Add(media.ReceivedFrame{SourceID: "peer", Sequence: sequence, Timestamp: uint32(sequence) * FrameSamples, Payload: quiet}); err != nil {
			t.Fatal(err)
		}
	}
	for range prebufferFrames {
		if _, _, err := mixerNext(mixer); err != nil {
			t.Fatal(err)
		}
	}
	stream := mixer.streams["peer"]
	measured := stream.normalizer.measuredRMS
	if measured == 0 {
		t.Fatal("real decoded frames did not train normalizer")
	}
	if err := mixer.Add(media.ReceivedFrame{SourceID: "peer", Sequence: 3, Timestamp: 4 * FrameSamples, Payload: loud}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mixerNext(mixer); err != nil {
		t.Fatal(err)
	}
	if stream.normalizer.measuredRMS != measured {
		t.Fatalf("synthetic frame changed measurement from %.6f to %.6f", measured, stream.normalizer.measuredRMS)
	}
	if _, _, err := mixerNext(mixer); err != nil {
		t.Fatal(err)
	}
	measured = stream.normalizer.measuredRMS
	if _, _, err := mixerNext(mixer); err != nil {
		t.Fatal(err)
	}
	if stream.normalizer.measuredRMS != measured {
		t.Fatalf("PLC frame changed measurement from %.6f to %.6f", measured, stream.normalizer.measuredRMS)
	}
	resetJitterStream(stream, [16]byte{1}, 0)
	if stream.normalizer.measuredRMS != 0 || stream.normalizer.gain != 1 || stream.normalizer.holdFrames != 0 {
		t.Fatalf("stream reset retained normalizer state: %#v", stream.normalizer)
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

func driveNormalizer(normalizer *loudnessNormalizer, level float64, measured, enabled bool, frames int) {
	pcm := make([]float32, FrameSamples)
	for range frames {
		for index := range pcm {
			pcm[index] = float32(level)
		}
		normalizer.process(pcm, level, measured, enabled)
	}
}

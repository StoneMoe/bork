package audio

import (
	"math"
	"testing"

	"bork/internal/media"
)

func TestMixerDoesNotTreatScreenAudioAsVoice(t *testing.T) {
	voiceEncoder, err := newOpusEncoder(4096)
	if err != nil {
		t.Fatal(err)
	}
	screenEncoder, err := newOpusEncoder(4096)
	if err != nil {
		t.Fatal(err)
	}
	voiceSamples := make([]float32, FrameSamples)
	screenSamples := make([]float32, FrameSamples)
	for index := range screenSamples {
		screenSamples[index] = 0.25 * float32(math.Sin(2*math.Pi*440*float64(index)/SampleRate))
	}

	mixer := newMixer(4096)
	mixer.setScreenAudioSource("peer")
	for sequence := uint64(1); sequence <= prebufferFrames; sequence++ {
		voicePayload, encodeErr := voiceEncoder.Encode(voiceSamples)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		screenPayload, encodeErr := screenEncoder.Encode(screenSamples)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		timestamp := uint32(sequence * FrameSamples)
		if err := mixer.Add(media.ReceivedFrame{
			SourceID: "peer", StreamKind: media.AudioStreamVoice, StreamID: [16]byte{1},
			Sequence: sequence, Timestamp: timestamp, Payload: voicePayload,
		}); err != nil {
			t.Fatal(err)
		}
		if err := mixer.Add(media.ReceivedFrame{
			SourceID: "peer", StreamKind: media.AudioStreamScreen, StreamID: [16]byte{2},
			Sequence: sequence, Timestamp: timestamp, Payload: screenPayload,
		}); err != nil {
			t.Fatal(err)
		}
	}

	mixed, err := mixer.NextInto(make([]float32, FrameSamples))
	if err != nil {
		t.Fatal(err)
	}
	if !mixed {
		t.Fatal("screen audio was not mixed into playback")
	}
	if peers := mixer.SpeakingPeerIDs(); len(peers) != 0 {
		t.Fatalf("screen audio marked peers as speaking: %v", peers)
	}
	screen := mixer.streams[mixerStreamKey{peerID: "peer", kind: media.AudioStreamScreen}]
	if screen == nil {
		t.Fatal("screen audio stream is missing")
	}
	if screen.normalizer.gain != 1 {
		t.Fatalf("screen audio loudness gain = %v, want 1", screen.normalizer.gain)
	}
}

func TestMixerScreenAudioSwitchKeepsVoice(t *testing.T) {
	mixer := newMixer(4096)
	mixer.setScreenAudioSource("first")
	voiceKey := mixerStreamKey{peerID: "first", kind: media.AudioStreamVoice}
	screenKey := mixerStreamKey{peerID: "first", kind: media.AudioStreamScreen}
	mixer.streams[voiceKey] = &jitterStream{}
	mixer.streams[screenKey] = &jitterStream{}

	mixer.setScreenAudioSource("second")
	if mixer.streams[voiceKey] == nil {
		t.Fatal("voice stream was removed during screen switch")
	}
	if mixer.streams[screenKey] != nil {
		t.Fatal("old screen stream survived screen switch")
	}
	if err := mixer.Add(media.ReceivedFrame{SourceID: "first", StreamKind: media.AudioStreamScreen, StreamID: [16]byte{2}, Sequence: 2, Payload: []byte{2}}); err != nil {
		t.Fatal(err)
	}
	if mixer.streams[screenKey] != nil {
		t.Fatal("unselected screen audio was added")
	}
}

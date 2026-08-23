package media

import "testing"

func TestFlowKeepsVoiceAndScreenAudioSeparate(t *testing.T) {
	flow := NewFlow()
	flow.SetScreenAudioSource("peer")
	voiceStream := [16]byte{1}
	screenStream := [16]byte{2}
	for sequence := uint64(1); sequence <= 2; sequence++ {
		if !flow.SubmitReceived(ReceivedFrame{
			SourceID: "peer", StreamKind: AudioStreamVoice, StreamID: voiceStream,
			Sequence: sequence, Payload: []byte{byte(sequence)},
		}) {
			t.Fatal("voice frame was rejected")
		}
		if !flow.SubmitReceived(ReceivedFrame{
			SourceID: "peer", StreamKind: AudioStreamScreen, StreamID: screenStream,
			Sequence: sequence, Payload: []byte{byte(sequence)},
		}) {
			t.Fatal("screen frame was rejected")
		}
	}

	counts := map[AudioStreamKind]int{}
	for range 4 {
		frame, ok := flow.TakeReceived()
		if !ok {
			t.Fatal("audio frame was lost when both streams used the same peer")
		}
		counts[frame.StreamKind]++
	}
	if counts[AudioStreamVoice] != 2 || counts[AudioStreamScreen] != 2 {
		t.Fatalf("received frame counts = %v", counts)
	}
}

func TestFlowScreenAudioFollowsSelectedSource(t *testing.T) {
	flow := NewFlow()
	flow.SetScreenAudioSource("first")
	if flow.ScreenAudioSource() != "first" {
		t.Fatal("selected screen audio source was not stored")
	}
	select {
	case <-flow.ReceivedReady():
	default:
		t.Fatal("screen audio selection did not wake playback")
	}
	if !flow.SubmitReceived(ReceivedFrame{SourceID: "first", StreamKind: AudioStreamScreen, StreamID: [16]byte{1}, Sequence: 1, Payload: []byte{1}}) {
		t.Fatal("selected screen audio was rejected")
	}
	if flow.SubmitReceived(ReceivedFrame{SourceID: "second", StreamKind: AudioStreamScreen, StreamID: [16]byte{2}, Sequence: 1, Payload: []byte{2}}) {
		t.Fatal("unselected screen audio was accepted")
	}
}

func TestFlowScreenAudioSwitchKeepsVoice(t *testing.T) {
	flow := NewFlow()
	flow.SetScreenAudioSource("first")
	if !flow.SubmitReceived(ReceivedFrame{SourceID: "first", StreamKind: AudioStreamScreen, StreamID: [16]byte{1}, Sequence: 1, Payload: []byte{1}}) {
		t.Fatal("selected screen audio was rejected")
	}
	if !flow.SubmitReceived(ReceivedFrame{SourceID: "first", StreamKind: AudioStreamVoice, StreamID: [16]byte{2}, Sequence: 1, Payload: []byte{2}}) {
		t.Fatal("voice frame was rejected")
	}
	flow.SetScreenAudioSource("second")
	voice, ok := flow.TakeReceived()
	if !ok || voice.StreamKind != AudioStreamVoice || voice.SourceID != "first" {
		t.Fatalf("voice frame was removed during screen switch: %#v", voice)
	}
	if screen, exists := flow.TakeReceived(); exists {
		t.Fatalf("old screen audio survived screen switch: %#v", screen)
	}
}

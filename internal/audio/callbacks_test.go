package audio

import (
	"sync/atomic"
	"testing"
)

func TestCaptureAssemblerAdvancesClockAcrossDropsAndMute(t *testing.T) {
	queue := newPCMFrameQueue(1)
	ready := make(chan struct{}, 1)
	assembler := captureAssembler{queue: queue, ready: ready}
	samples := make([]float32, FrameSamples)
	for index := range samples {
		samples[index] = 0.5
	}
	reference := make([]float32, FrameSamples)
	for index := range reference {
		reference[index] = 0.25
	}
	assembler.Write(samples[:FrameSamples/2], reference[:FrameSamples/2], false)
	assembler.Write(samples[FrameSamples/2:], reference[FrameSamples/2:], false)
	assembler.Write(samples, reference, false) // full queue: timestamp 960 is dropped
	first, ok := queue.AcquireRead()
	if !ok || first.Timestamp != 480 || first.Muted || !first.ReferenceValid || first.Reference[0] != 0.25 || first.Reference[FrameSamples-1] != 0.25 {
		t.Fatalf("first timestamp = %d, %v", first.Timestamp, ok)
	}
	queue.ReleaseRead()
	assembler.Write(samples, reference, true)
	mutedFrame, ok := queue.AcquireRead()
	if !ok || mutedFrame.Timestamp != 1440 || !mutedFrame.Muted || !mutedFrame.ReferenceValid || mutedFrame.Samples[0] != 0 || mutedFrame.Reference[0] != 0.25 {
		t.Fatalf("muted frame = %#v, %v", mutedFrame, ok)
	}
	queue.ReleaseRead()
	assembler.Write(samples, nil, false) // next delivered timestamp remains on the sample clock
	next, ok := queue.AcquireRead()
	if !ok || next.Timestamp != 1920 || next.Muted || next.ReferenceValid || next.Reference[0] != 0 {
		t.Fatalf("next timestamp = %d, %v", next.Timestamp, ok)
	}
}

func TestPlaybackReaderHoldsFrameAcrossCallbacksAndRequestsAhead(t *testing.T) {
	queue := newPCMFrameQueue(1)
	frame, _ := queue.AcquireWrite()
	frame.Index = 0
	for index := range frame.Samples {
		frame.Samples[index] = 0.5
	}
	queue.CommitWrite()
	wake := make(chan struct{}, 1)
	var demand atomic.Uint64
	var playbackMuted atomic.Bool
	reader := newPlaybackReader(queue, wake, &demand, &playbackMuted)
	firstHalf := make([]float32, FrameSamples/2)
	secondHalf := make([]float32, FrameSamples/2)
	reader.Read(firstHalf)
	if demand.Load() != 1 {
		t.Fatalf("playback demand = %d, want 1", demand.Load())
	}
	if _, ok := queue.AcquireWrite(); ok {
		t.Fatal("producer reused the frame while callback still held it")
	}
	reader.Read(secondHalf)
	if _, ok := queue.AcquireWrite(); !ok {
		t.Fatal("callback did not release the completed frame")
	}
	for _, sample := range append(firstHalf, secondHalf...) {
		if sample != 0.5 {
			t.Fatalf("playback sample = %f", sample)
		}
	}
}

func TestPlaybackReaderOutputsSilenceInsteadOfFutureFrame(t *testing.T) {
	queue := newPCMFrameQueue(2)
	frame, _ := queue.AcquireWrite()
	frame.Index = 1
	for index := range frame.Samples {
		frame.Samples[index] = 0.5
	}
	queue.CommitWrite()
	var demand atomic.Uint64
	var playbackMuted atomic.Bool
	reader := newPlaybackReader(queue, make(chan struct{}, 1), &demand, &playbackMuted)
	output := make([]float32, FrameSamples)
	reader.Read(output)
	for _, sample := range output {
		if sample != 0 {
			t.Fatal("future frame was played before its index")
		}
	}
	reader.Read(output)
	if output[0] != 0.5 {
		t.Fatal("future frame was not played at its scheduled index")
	}
}

func TestPlaybackMuteDrainsCurrentTimeline(t *testing.T) {
	queue := newPCMFrameQueue(1)
	frame, _ := queue.AcquireWrite()
	frame.Index = 0
	for index := range frame.Samples {
		frame.Samples[index] = 0.5
	}
	queue.CommitWrite()
	var demand atomic.Uint64
	var playbackMuted atomic.Bool
	playbackMuted.Store(true)
	reader := newPlaybackReader(queue, make(chan struct{}, 1), &demand, &playbackMuted)
	output := make([]float32, FrameSamples)
	reader.Read(output)
	for _, sample := range output {
		if sample != 0 {
			t.Fatal("muted playback emitted audio")
		}
	}

	next, ok := queue.AcquireWrite()
	if !ok {
		t.Fatal("muted playback retained the consumed frame")
	}
	next.Index = 1
	next.LocalOnly = true
	for index := range next.Samples {
		next.Samples[index] = 0.25
	}
	queue.CommitWrite()
	reader.Read(output)
	if output[0] != 0.25 || demand.Load() != 2 {
		t.Fatalf("local-only playback while muted = %f, demand %d", output[0], demand.Load())
	}

	next, _ = queue.AcquireWrite()
	next.Index = 2
	next.LocalOnly = false
	for index := range next.Samples {
		next.Samples[index] = 0.75
	}
	queue.CommitWrite()
	reader.Read(output)
	if output[0] != 0 {
		t.Fatalf("ordinary playback leaked after local-only frame: %f", output[0])
	}

	next, _ = queue.AcquireWrite()
	next.Index = 3
	next.LocalOnly = false
	for index := range next.Samples {
		next.Samples[index] = 0.5
	}
	queue.CommitWrite()
	playbackMuted.Store(false)
	reader.Read(output)
	if output[0] != 0.5 || demand.Load() != 4 {
		t.Fatalf("playback timeline after unmute = %f, demand %d", output[0], demand.Load())
	}
}

func TestGainRampSpansOneFrameAndClamps(t *testing.T) {
	samples := make([]float32, FrameSamples)
	for index := range samples {
		samples[index] = 0.75
	}
	if final := applyGainRamp(samples, 1, 2); final != 2 {
		t.Fatalf("final gain = %f, want 2", final)
	}
	if samples[0] <= 0.75 || samples[0] >= samples[len(samples)-1] {
		t.Fatalf("gain did not ramp across the frame: first=%f last=%f", samples[0], samples[len(samples)-1])
	}
	if samples[len(samples)-1] != 1 {
		t.Fatalf("positive gain was not clamped: %f", samples[len(samples)-1])
	}
	for index := range samples {
		samples[index] = -0.75
	}
	applyGainRamp(samples, 2, 2)
	if samples[0] != -1 || samples[len(samples)-1] != -1 {
		t.Fatal("negative gain was not clamped")
	}
}

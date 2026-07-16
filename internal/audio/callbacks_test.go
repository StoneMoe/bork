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
	assembler.Write(samples[:FrameSamples/2], false)
	assembler.Write(samples[FrameSamples/2:], false)
	assembler.Write(samples, false) // full queue: timestamp 960 is dropped
	first, ok := queue.AcquireRead()
	if !ok || first.Timestamp != 480 {
		t.Fatalf("first timestamp = %d, %v", first.Timestamp, ok)
	}
	queue.ReleaseRead()
	assembler.Write(samples, true)
	mutedFrame, ok := queue.AcquireRead()
	if !ok || mutedFrame.Timestamp != 1440 || mutedFrame.Samples[0] != 0 {
		t.Fatalf("muted frame = %#v, %v", mutedFrame, ok)
	}
	queue.ReleaseRead()
	assembler.Write(samples, false) // next delivered timestamp remains on the sample clock
	next, ok := queue.AcquireRead()
	if !ok || next.Timestamp != 1920 {
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
	reader := newPlaybackReader(queue, wake, &demand)
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
	reader := newPlaybackReader(queue, make(chan struct{}, 1), &demand)
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

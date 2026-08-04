package media

import (
	"fmt"
	"testing"
	"time"
)

func TestFlowBoundsReceivedPerSourceAndPreservesFairness(t *testing.T) {
	flow := NewFlow()
	for sequence := uint64(1); sequence <= 4; sequence++ {
		flow.SubmitReceived(ReceivedFrame{SourceID: "noisy", Sequence: sequence, Payload: []byte{byte(sequence)}})
	}
	flow.SubmitReceived(ReceivedFrame{SourceID: "other", Sequence: 1, Payload: []byte{1}})
	first, ok := flow.TakeReceived()
	if !ok || first.SourceID != "noisy" || first.Sequence != 3 {
		t.Fatalf("first frame = %#v, %v", first, ok)
	}
	second, ok := flow.TakeReceived()
	if !ok || second.SourceID != "other" {
		t.Fatalf("second frame = %#v, %v", second, ok)
	}
}

func TestFlowAcceptsMoreThanSixteenSources(t *testing.T) {
	flow := NewFlow()
	for index := range 32 {
		if !flow.SubmitReceived(ReceivedFrame{SourceID: fmt.Sprintf("peer-%d", index), Sequence: 1, Payload: []byte{1}}) {
			t.Fatalf("source %d was rejected", index)
		}
	}
}

func TestFlowKeepsNewestSendFrame(t *testing.T) {
	flow := NewFlow()
	generation := flow.Reset()
	flow.SubmitSend(SendFrame{Timestamp: 480, Payload: []byte{1}, Generation: generation})
	flow.SubmitSend(SendFrame{Timestamp: 960, Payload: []byte{2}, Generation: generation})
	var frame SendFrame
	ok := flow.ConsumeSend(func(current SendFrame) { frame = current })
	if !ok || frame.Timestamp != 960 || frame.Payload[0] != 2 {
		t.Fatalf("send frame = %#v, %v", frame, ok)
	}
	if flow.ConsumeSend(func(SendFrame) {}) {
		t.Fatal("ConsumeSend() returned more than the newest frame")
	}
}

func TestFlowResetReleasesQueuedFrames(t *testing.T) {
	flow := NewFlow()
	flow.SubmitReceived(ReceivedFrame{SourceID: "peer", Sequence: 1, Payload: []byte{1}})
	generation := flow.Reset()
	flow.SubmitReceived(ReceivedFrame{SourceID: "peer", Sequence: 1, Payload: []byte{1}})
	flow.SubmitSend(SendFrame{Timestamp: 480, Payload: []byte{2}, Generation: generation})
	flow.Reset()
	if _, ok := flow.TakeReceived(); ok {
		t.Fatal("received frame survived Reset()")
	}
	if flow.ConsumeSend(func(SendFrame) {}) {
		t.Fatal("send frame survived Reset()")
	}
}

func TestFlowDoesNotReplaceNewReceivedFramesWithLatePacket(t *testing.T) {
	flow := NewFlow()
	for _, sequence := range []uint64{10, 11, 9} {
		flow.SubmitReceived(ReceivedFrame{SourceID: "peer", Sequence: sequence, Payload: []byte{byte(sequence)}})
	}
	first, _ := flow.TakeReceived()
	second, _ := flow.TakeReceived()
	if first.Sequence != 10 || second.Sequence != 11 {
		t.Fatalf("retained sequences = %d, %d", first.Sequence, second.Sequence)
	}
}

func TestSendInvalidationWaitsForConsumerLease(t *testing.T) {
	flow := NewFlow()
	generation := flow.Reset()
	if !flow.SubmitSend(SendFrame{Payload: []byte{1}, Generation: generation}) {
		t.Fatal("SubmitSend() failed")
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	consumed := make(chan struct{})
	go func() {
		flow.ConsumeSend(func(SendFrame) {
			close(entered)
			<-release
		})
		close(consumed)
	}()
	<-entered
	invalidated := make(chan uint64, 1)
	go func() { invalidated <- flow.InvalidateSend() }()
	select {
	case <-invalidated:
		t.Fatal("InvalidateSend() returned while consumer held the lease")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-consumed
	newGeneration := <-invalidated
	if flow.SubmitSend(SendFrame{Payload: []byte{2}, Generation: generation}) {
		t.Fatal("stale send generation was accepted")
	}
	if !flow.SubmitSend(SendFrame{Payload: []byte{3}, Generation: newGeneration}) {
		t.Fatal("current send generation was rejected")
	}
}

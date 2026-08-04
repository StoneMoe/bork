package peer

import (
	"errors"
	"testing"
)

func TestControlFlowReplayWindow(t *testing.T) {
	var control controlFlow
	if !control.mayReceive(64) || !control.mayReceive(64) {
		t.Fatal("checking a sequence mutated the receive window")
	}
	if !control.commitReceived(64) || control.mayReceive(64) {
		t.Fatal("committed sequence was replayable")
	}
	if !control.commitReceived(1) || !control.commitReceived(65) {
		t.Fatal("valid out-of-order or advancing sequence was rejected")
	}
	if control.mayReceive(1) || control.mayReceive(0) {
		t.Fatal("stale or zero sequence was accepted")
	}
}

func TestControlFlowSequenceDoesNotWrap(t *testing.T) {
	var control controlFlow
	control.sendSequence = ^uint64(0) - 1
	if sequence, err := control.nextSendSequence(); err != nil || sequence != ^uint64(0) {
		t.Fatalf("last sequence = %d, %v", sequence, err)
	}
	if _, err := control.nextSendSequence(); !errors.Is(err, errSequenceExhausted) {
		t.Fatalf("overflow error = %v", err)
	}
}

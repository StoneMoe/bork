package link

import (
	"errors"
	"testing"
	"time"
)

func TestControlAndMediaFlowsUseIndependentReceiveWindows(t *testing.T) {
	var control ControlFlow
	media := NewMediaFlow(time.Unix(1, 0))
	if !control.MayReceive(1) || !control.CommitReceived(1) ||
		!media.MayReceive(1) || !media.CommitReceived(1) || !media.AllowReceived(time.Unix(1, 0)) {
		t.Fatal("initial sequences were rejected")
	}
	if control.MayReceive(1) || media.MayReceive(1) {
		t.Fatal("duplicate sequences were accepted")
	}
}

func TestMediaFlowCommitsRateLimitedSequence(t *testing.T) {
	now := time.Unix(1, 0)
	media := NewMediaFlow(now)
	media.rateTokens = 0
	if !media.MayReceive(100) || !media.CommitReceived(100) {
		t.Fatal("authenticated sequence was rejected")
	}
	if media.AllowReceived(now) {
		t.Fatal("packet was accepted without a rate token")
	}
	if media.MayReceive(100) {
		t.Fatal("rate-limited packet was replayable")
	}
}

func TestFlowSequenceDoesNotWrap(t *testing.T) {
	var control ControlFlow
	control.sendSequence = ^uint64(0) - 1
	if sequence, err := control.NextSendSequence(); err != nil || sequence != ^uint64(0) {
		t.Fatalf("last sequence = %d, %v", sequence, err)
	}
	if _, err := control.NextSendSequence(); !errors.Is(err, ErrSequenceExhausted) {
		t.Fatalf("overflow error = %v", err)
	}
}

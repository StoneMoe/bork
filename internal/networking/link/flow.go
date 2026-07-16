package link

import (
	"errors"
	"math"
	"time"
)

const (
	defaultMediaPacketsPerSecond = 110.0
	defaultMediaPacketBurst      = 20.0
)

var ErrSequenceExhausted = errors.New("link sequence is exhausted")

type sequenceWindow struct {
	highest uint64
	seen    uint64
}

func (w *sequenceWindow) accept(sequence uint64) bool {
	if sequence == 0 {
		return false
	}
	if sequence > w.highest {
		shift := sequence - w.highest
		if shift >= 64 {
			w.seen = 1
		} else {
			w.seen = (w.seen << shift) | 1
		}
		w.highest = sequence
		return true
	}
	delta := w.highest - sequence
	if delta >= 64 {
		return false
	}
	mask := uint64(1) << delta
	if w.seen&mask != 0 {
		return false
	}
	w.seen |= mask
	return true
}

func (w sequenceWindow) mayAccept(sequence uint64) bool {
	return w.accept(sequence)
}

// ControlFlow is owned by one room protocol loop and is not concurrency-safe.
type ControlFlow struct {
	sendSequence  uint64
	receiveWindow sequenceWindow
}

func (f *ControlFlow) NextSendSequence() (uint64, error) {
	if f.sendSequence == math.MaxUint64 {
		return 0, ErrSequenceExhausted
	}
	f.sendSequence++
	return f.sendSequence, nil
}

func (f *ControlFlow) MayReceive(sequence uint64) bool {
	return f.receiveWindow.mayAccept(sequence)
}

func (f *ControlFlow) CommitReceived(sequence uint64) bool {
	return f.receiveWindow.accept(sequence)
}

// MediaFlow is owned by one room protocol loop and is not concurrency-safe.
type MediaFlow struct {
	sendSequence     uint64
	receiveWindow    sequenceWindow
	packetsPerSecond float64
	packetBurst      float64
	rateTokens       float64
	rateUpdatedAt    time.Time
}

func NewMediaFlow(now time.Time) *MediaFlow {
	return &MediaFlow{
		packetsPerSecond: defaultMediaPacketsPerSecond,
		packetBurst:      defaultMediaPacketBurst,
		rateTokens:       defaultMediaPacketBurst,
		rateUpdatedAt:    now,
	}
}

func (f *MediaFlow) NextSendSequence() (uint64, error) {
	if f.sendSequence == math.MaxUint64 {
		return 0, ErrSequenceExhausted
	}
	f.sendSequence++
	return f.sendSequence, nil
}

func (f *MediaFlow) MayReceive(sequence uint64) bool {
	return f.receiveWindow.mayAccept(sequence)
}

func (f *MediaFlow) CommitReceived(sequence uint64) bool {
	return f.receiveWindow.accept(sequence)
}

func (f *MediaFlow) AllowReceived(now time.Time) bool {
	if f.rateUpdatedAt.IsZero() {
		f.rateUpdatedAt = now
		f.rateTokens = f.packetBurst
	}
	elapsed := now.Sub(f.rateUpdatedAt).Seconds()
	f.rateUpdatedAt = now
	f.rateTokens = min(f.packetBurst, f.rateTokens+elapsed*f.packetsPerSecond)
	if f.rateTokens < 1 {
		return false
	}
	f.rateTokens--
	return true
}

package peer

import (
	"errors"
	"math"
)

var errSequenceExhausted = errors.New("session sequence is exhausted")

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

type controlFlow struct {
	sendSequence  uint64
	receiveWindow sequenceWindow
}

func (f *controlFlow) nextSendSequence() (uint64, error) {
	if f.sendSequence == math.MaxUint64 {
		return 0, errSequenceExhausted
	}
	f.sendSequence++
	return f.sendSequence, nil
}

func (f *controlFlow) mayReceive(sequence uint64) bool {
	return f.receiveWindow.mayAccept(sequence)
}

func (f *controlFlow) commitReceived(sequence uint64) bool {
	return f.receiveWindow.accept(sequence)
}

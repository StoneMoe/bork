package iwan

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

func TestReassembler_matchesPinnedFragmentVectors(t *testing.T) {
	session := goldenSession(t)
	reassembler := NewReassembler()
	first := mustDecodeHex(t, "21001234deadbeef00000000000000007856341200000300ca98dbd31af8")
	last := mustDecodeHex(t, "21001234deadbeef00000000000000007856341219800200133bd091d3")
	now := time.Unix(100, 0)

	payload, complete, err := reassembler.Push(first, session, now)
	if err != nil || complete || payload != nil {
		t.Fatalf("first Push() = %x, %v, %v", payload, complete, err)
	}
	payload, complete, err = reassembler.Push(last, session, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !complete || !bytes.Equal(payload, []byte("hello world")) {
		t.Fatalf("last Push() = %q, %v", payload, complete)
	}
}

func TestReassembler_rejectsOverlapConflictAndOverflow(t *testing.T) {
	session := goldenSession(t)
	now := time.Unix(100, 0)
	tests := []struct {
		name   string
		first  []byte
		second []byte
		want   error
	}{
		{name: "overlap", first: fragmentPacket(session, 1, 0, false, []byte("abcd")), second: fragmentPacket(session, 1, 2, true, []byte("cdef")), want: ErrFragmentOverlap},
		{name: "conflicting final length", first: fragmentPacket(session, 2, 4, true, []byte("ef")), second: fragmentPacket(session, 2, 0, true, []byte("abcd")), want: ErrFragmentConflict},
		{name: "overflow", first: fragmentPacket(session, 3, 4090, true, []byte("0123456789")), want: ErrFragmentOverflow},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reassembler := NewReassembler()
			_, _, err := reassembler.Push(test.first, session, now)
			if test.second != nil && err == nil {
				_, _, err = reassembler.Push(test.second, session, now)
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("Push() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestReassembler_boundsSlotsAndExpiresThem(t *testing.T) {
	session := goldenSession(t)
	reassembler := NewReassembler()
	now := time.Unix(100, 0)
	for id := uint32(1); id <= FragmentSlots; id++ {
		if _, _, err := reassembler.Push(fragmentPacket(session, id, 0, false, []byte{byte(id)}), session, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := reassembler.Push(fragmentPacket(session, 11, 0, false, []byte("x")), session, now); !errors.Is(err, ErrFragmentSlotsFull) {
		t.Fatalf("Push() error = %v, want ErrFragmentSlotsFull", err)
	}
	if _, _, err := reassembler.Push(fragmentPacket(session, 11, 0, false, []byte("x")), session, now.Add(FragmentTimeout)); err != nil {
		t.Fatalf("expired slot was not reused: %v", err)
	}
}

func TestReassembler_rejectsSessionMismatchAndMalformedMetadata(t *testing.T) {
	session := goldenSession(t)
	packet := fragmentPacket(session, 1, 0, false, []byte("x"))
	reassembler := NewReassembler()

	packet[2] ^= 1
	if _, _, err := reassembler.Push(packet, session, time.Unix(100, 0)); !errors.Is(err, ErrSessionMismatch) {
		t.Fatalf("Push() error = %v, want ErrSessionMismatch", err)
	}
	packet = fragmentPacket(session, 1, 0, false, []byte("x"))
	bitfield := binary.LittleEndian.Uint32(packet[20:24]) | 2
	binary.LittleEndian.PutUint32(packet[20:24], bitfield)
	if _, _, err := reassembler.Push(packet, session, time.Unix(100, 0)); err == nil {
		t.Fatal("Push() accepted reserved metadata bit")
	}
}

func fragmentPacket(session Session, id uint32, offset int, final bool, payload []byte) []byte {
	packet := make([]byte, fragmentHeaderSize+len(payload))
	packet[0] = byte(TypeIPFragment)
	copy(packet[2:4], session.Token[:])
	copy(packet[4:8], session.ID[:])
	binary.LittleEndian.PutUint32(packet[16:20], id)
	bits := uint32(offset)<<2 | uint32(len(payload))<<15
	if final {
		bits |= 1
	}
	binary.LittleEndian.PutUint32(packet[20:24], bits)
	copy(packet[24:], payload)
	return packet
}

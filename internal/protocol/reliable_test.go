package protocol

import (
	"bytes"
	"crypto/cipher"
	"math"
	"reflect"
	"testing"

	"golang.org/x/crypto/chacha20poly1305"
)

func TestReliableRoundTripOrderedMultiFragment(t *testing.T) {
	protector := newReliableTestProtector(t, 1)
	roomTag := [16]byte{1, 2, 3}
	sessionID := [16]byte{4, 5, 6}
	packetSequence := uint64(41)
	want := ReliablePacket{
		Channel:          7,
		Flags:            ReliableFlagOrdered,
		FragmentSequence: 101,
		MessageSequence:  22,
		FragmentIndex:    2,
		FragmentCount:    4,
		AckBase:          80,
		AckBitmap:        1 | 1<<3 | 1<<63,
		Payload:          []byte("reliable fragment payload"),
	}

	packet, err := MarshalReliable(roomTag, sessionID, packetSequence, want, protector)
	if err != nil {
		t.Fatal(err)
	}
	if len(packet) > MaxBridgeInnerSize {
		t.Fatalf("reliable packet length = %d", len(packet))
	}
	if bytes.Contains(packet[establishedHeaderSize:], want.Payload) {
		t.Fatal("reliable payload remained visible in ciphertext")
	}
	header, err := ParseEstablishedHeader(packet)
	if err != nil || header.Sequence != packetSequence {
		t.Fatalf("reliable header = %#v, %v", header, err)
	}
	got, err := ParseReliable(packet, roomTag, sessionID, protector)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Ordered() || got.AckOnly() || !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded reliable packet = %#v, want %#v", got, want)
	}
}

func TestReliableAckOnlyRoundTrip(t *testing.T) {
	protector := newReliableTestProtector(t, 2)
	roomTag := [16]byte{7}
	sessionID := [16]byte{8}
	packetSequence := uint64(19)
	want := ReliablePacket{
		Channel:   3,
		Flags:     ReliableFlagAckOnly,
		AckBase:   5,
		AckBitmap: 1 | 1<<2,
	}

	packet, err := MarshalReliable(roomTag, sessionID, packetSequence, want, protector)
	if err != nil {
		t.Fatal(err)
	}
	wantSize := establishedHeaderSize + reliablePlaintextFixedSize + aeadTagSize
	if len(packet) != wantSize {
		t.Fatalf("ACK-only packet length = %d, want %d", len(packet), wantSize)
	}
	got, err := ParseReliable(packet, roomTag, sessionID, protector)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AckOnly() || got.Ordered() || !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded ACK-only packet = %#v, want %#v", got, want)
	}
}

func TestReliableRejectsWrongKeyAndTampering(t *testing.T) {
	protector := newReliableTestProtector(t, 3)
	wrongProtector := newReliableTestProtector(t, 4)
	roomTag := [16]byte{9}
	sessionID := [16]byte{10}
	p := validReliableTestPacket()
	packet, err := MarshalReliable(roomTag, sessionID, 1, p, protector)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseReliable(bytes.Clone(packet), roomTag, sessionID, wrongProtector); err == nil {
		t.Fatal("ParseReliable() accepted the wrong key")
	}
	tampered := bytes.Clone(packet)
	tampered[len(tampered)-1] ^= 1
	if _, err := ParseReliable(tampered, roomTag, sessionID, protector); err == nil {
		t.Fatal("ParseReliable() accepted tampered ciphertext")
	}
}

func TestReliableRejectsInvalidFields(t *testing.T) {
	protector := newReliableTestProtector(t, 5)
	roomTag := [16]byte{11}
	sessionID := [16]byte{12}
	tests := []struct {
		name           string
		packetSequence uint64
		change         func(*ReliablePacket)
	}{
		{name: "zero packet sequence", packetSequence: 0, change: func(*ReliablePacket) {}},
		{name: "zero channel", packetSequence: 1, change: func(p *ReliablePacket) { p.Channel = 0 }},
		{name: "unknown flag", packetSequence: 1, change: func(p *ReliablePacket) { p.Flags = 1 << 7 }},
		{name: "zero fragment sequence", packetSequence: 1, change: func(p *ReliablePacket) { p.FragmentSequence = 0 }},
		{name: "zero message sequence", packetSequence: 1, change: func(p *ReliablePacket) { p.MessageSequence = 0 }},
		{name: "zero fragment count", packetSequence: 1, change: func(p *ReliablePacket) { p.FragmentCount = 0 }},
		{name: "too many fragments", packetSequence: 1, change: func(p *ReliablePacket) { p.FragmentCount = MaxReliableFragments + 1 }},
		{name: "fragment index at count", packetSequence: 1, change: func(p *ReliablePacket) { p.FragmentIndex = p.FragmentCount }},
		{name: "empty data", packetSequence: 1, change: func(p *ReliablePacket) { p.Payload = nil }},
		{name: "oversized data", packetSequence: 1, change: func(p *ReliablePacket) { p.Payload = make([]byte, MaxReliablePayload+1) }},
		{name: "bitmap with zero base", packetSequence: 1, change: func(p *ReliablePacket) { p.AckBase, p.AckBitmap = 0, 1 }},
		{name: "base without bitmap", packetSequence: 1, change: func(p *ReliablePacket) { p.AckBase, p.AckBitmap = 1, 0 }},
		{name: "bitmap omits base", packetSequence: 1, change: func(p *ReliablePacket) { p.AckBase, p.AckBitmap = 2, 2 }},
		{name: "bitmap includes sequence zero", packetSequence: 1, change: func(p *ReliablePacket) { p.AckBase, p.AckBitmap = 1, 3 }},
		{name: "ordered ACK-only", packetSequence: 1, change: func(p *ReliablePacket) { *p = validReliableAckTestPacket(); p.Flags |= ReliableFlagOrdered }},
		{name: "ACK-only fragment sequence", packetSequence: 1, change: func(p *ReliablePacket) { *p = validReliableAckTestPacket(); p.FragmentSequence = 1 }},
		{name: "ACK-only message sequence", packetSequence: 1, change: func(p *ReliablePacket) { *p = validReliableAckTestPacket(); p.MessageSequence = 1 }},
		{name: "ACK-only fragment index", packetSequence: 1, change: func(p *ReliablePacket) { *p = validReliableAckTestPacket(); p.FragmentIndex = 1 }},
		{name: "ACK-only fragment count", packetSequence: 1, change: func(p *ReliablePacket) { *p = validReliableAckTestPacket(); p.FragmentCount = 1 }},
		{name: "ACK-only payload", packetSequence: 1, change: func(p *ReliablePacket) { *p = validReliableAckTestPacket(); p.Payload = []byte{1} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := validReliableTestPacket()
			test.change(&p)
			if _, err := MarshalReliable(roomTag, sessionID, test.packetSequence, p, protector); err == nil {
				t.Fatal("MarshalReliable() accepted invalid fields")
			}
		})
	}

	if _, err := MarshalReliable(roomTag, sessionID, 1, validReliableTestPacket(), nil); err == nil {
		t.Fatal("MarshalReliable() accepted a nil protector")
	}
	if _, err := ParseReliable(make([]byte, establishedHeaderSize+reliablePlaintextFixedSize+aeadTagSize-1), roomTag, sessionID, protector); err == nil {
		t.Fatal("ParseReliable() accepted a truncated packet")
	}
	if _, err := ParseReliable(make([]byte, MaxBridgeInnerSize+1), roomTag, sessionID, protector); err == nil {
		t.Fatal("ParseReliable() accepted an oversized packet")
	}
}

func TestAckContainsBoundaries(t *testing.T) {
	bitmap := uint64(1) | 1<<1 | 1<<63
	tests := []struct {
		name     string
		base     uint64
		bitmap   uint64
		sequence uint64
		want     bool
	}{
		{name: "base", base: 100, bitmap: bitmap, sequence: 100, want: true},
		{name: "previous", base: 100, bitmap: bitmap, sequence: 99, want: true},
		{name: "gap", base: 100, bitmap: bitmap, sequence: 98, want: false},
		{name: "oldest represented", base: 100, bitmap: bitmap, sequence: 37, want: true},
		{name: "delta 64", base: 100, bitmap: math.MaxUint64, sequence: 36, want: false},
		{name: "newer than base", base: 100, bitmap: math.MaxUint64, sequence: 101, want: false},
		{name: "zero sequence", base: 100, bitmap: math.MaxUint64, sequence: 0, want: false},
		{name: "zero base", base: 0, bitmap: math.MaxUint64, sequence: 1, want: false},
		{name: "maximum base", base: math.MaxUint64, bitmap: 1, sequence: math.MaxUint64, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := AckContains(test.base, test.bitmap, test.sequence); got != test.want {
				t.Fatalf("AckContains(%d, %#x, %d) = %t, want %t", test.base, test.bitmap, test.sequence, got, test.want)
			}
		})
	}
}

func newReliableTestProtector(t testing.TB, marker byte) cipher.AEAD {
	t.Helper()
	key := make([]byte, chacha20poly1305.KeySize)
	key[0] = marker
	protector, err := chacha20poly1305.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return protector
}

func validReliableTestPacket() ReliablePacket {
	return ReliablePacket{
		Channel:          1,
		FragmentSequence: 1,
		MessageSequence:  1,
		FragmentCount:    1,
		AckBase:          64,
		AckBitmap:        1,
		Payload:          []byte{1},
	}
}

func validReliableAckTestPacket() ReliablePacket {
	return ReliablePacket{
		Channel:   1,
		Flags:     ReliableFlagAckOnly,
		AckBase:   1,
		AckBitmap: 1,
	}
}

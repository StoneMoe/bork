package protocol

import (
	"bytes"
	"testing"
)

func TestBridgeControlRoundTrip(t *testing.T) {
	pair := newTestSessionPair(t)
	origin := [32]byte{1}
	target := [32]byte{2}
	inner, err := MarshalControl(PacketPing, pair.roomTag, pair.firstMaterial.SessionID, 11, 99, pair.firstMaterial.Ciphers.ControlSend)
	if err != nil {
		t.Fatal(err)
	}
	packet, err := MarshalBridge(pair.roomTag, pair.firstMaterial.SessionID, 12, origin, target, false, inner, pair.firstMaterial.Ciphers.ControlSend)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(packet[establishedHeaderSize:], inner) {
		t.Fatal("bridge body remained visible in ciphertext")
	}
	header, err := ParseEstablishedHeader(packet)
	if err != nil || header.Sequence != 12 {
		t.Fatalf("bridge header = %#v, %v", header, err)
	}
	decoded, err := ParseBridge(packet, pair.roomTag, pair.secondMaterial.SessionID, pair.secondMaterial.Ciphers.ControlRecv)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Origin != origin || decoded.Target != target || !bytes.Equal(decoded.Inner, inner) {
		t.Fatalf("decoded bridge = %+v", decoded)
	}
}

func TestBridgeAcceptsMaximumReliableInner(t *testing.T) {
	pair := newTestSessionPair(t)
	reliable, err := MarshalReliable(pair.roomTag, pair.firstMaterial.SessionID, 1, ReliablePacket{
		Channel: 1, FragmentSequence: 1, MessageSequence: 1, FragmentCount: 1,
		Payload: make([]byte, MaxReliablePayload),
	}, pair.firstMaterial.Ciphers.ControlSend)
	if err != nil {
		t.Fatal(err)
	}
	if len(reliable) != MaxBridgeInnerSize {
		t.Fatalf("reliable packet size = %d, want %d", len(reliable), MaxBridgeInnerSize)
	}
	packet, err := MarshalBridge(pair.roomTag, pair.firstMaterial.SessionID, 2, [32]byte{1}, [32]byte{2}, true, reliable, pair.firstMaterial.Ciphers.ControlSend)
	if err != nil {
		t.Fatal(err)
	}
	if len(packet) != MaxDatagramSize {
		t.Fatalf("bridge packet size = %d, want %d", len(packet), MaxDatagramSize)
	}
	decoded, err := ParseBridge(packet, pair.roomTag, pair.secondMaterial.SessionID, pair.secondMaterial.Ciphers.ControlRecv)
	if err != nil || !decoded.Background {
		t.Fatalf("background bridge = %#v, %v", decoded, err)
	}
}

func TestBridgeRejectsInvalidFieldsAndTampering(t *testing.T) {
	pair := newTestSessionPair(t)
	inner, err := MarshalControl(PacketPing, pair.roomTag, pair.firstMaterial.SessionID, 1, 2, pair.firstMaterial.Ciphers.ControlSend)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MarshalBridge(pair.roomTag, pair.firstMaterial.SessionID, 2, [32]byte{}, [32]byte{2}, false, inner, pair.firstMaterial.Ciphers.ControlSend); err == nil {
		t.Fatal("zero origin was accepted")
	}
	if _, err := MarshalBridge(pair.roomTag, pair.firstMaterial.SessionID, 2, [32]byte{1}, [32]byte{1}, false, inner, pair.firstMaterial.Ciphers.ControlSend); err == nil {
		t.Fatal("equal endpoints were accepted")
	}
	wrongType := bytes.Clone(inner)
	wrongType[5] = 255
	if _, err := MarshalBridge(pair.roomTag, pair.firstMaterial.SessionID, 2, [32]byte{1}, [32]byte{2}, false, wrongType, pair.firstMaterial.Ciphers.ControlSend); err == nil {
		t.Fatal("invalid inner type was accepted")
	}
	if _, err := MarshalBridge(pair.roomTag, pair.firstMaterial.SessionID, 2, [32]byte{1}, [32]byte{2}, true, inner, pair.firstMaterial.Ciphers.ControlSend); err == nil {
		t.Fatal("non-reliable background inner was accepted")
	}
	packet, err := MarshalBridge(pair.roomTag, pair.firstMaterial.SessionID, 2, [32]byte{1}, [32]byte{2}, false, inner, pair.firstMaterial.Ciphers.ControlSend)
	if err != nil {
		t.Fatal(err)
	}
	packet[len(packet)-1] ^= 1
	if _, err := ParseBridge(packet, pair.roomTag, pair.secondMaterial.SessionID, pair.secondMaterial.Ciphers.ControlRecv); err == nil {
		t.Fatal("tampered bridge was accepted")
	}
	if _, err := ParseBridge(packet, pair.roomTag, pair.secondMaterial.SessionID, nil); err == nil {
		t.Fatal("nil protector was accepted")
	}
}

func FuzzParseBridge(f *testing.F) {
	pair := newTestSessionPair(f)
	inner, err := MarshalControl(PacketPing, pair.roomTag, pair.firstMaterial.SessionID, 1, 2, pair.firstMaterial.Ciphers.ControlSend)
	if err != nil {
		f.Fatal(err)
	}
	packet, err := MarshalBridge(pair.roomTag, pair.firstMaterial.SessionID, 2, [32]byte{1}, [32]byte{2}, false, inner, pair.firstMaterial.Ciphers.ControlSend)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(packet)
	f.Fuzz(func(t *testing.T, candidate []byte) {
		_, _ = ParseBridge(candidate, pair.roomTag, pair.secondMaterial.SessionID, pair.secondMaterial.Ciphers.ControlRecv)
	})
}

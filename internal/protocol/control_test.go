package protocol

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

type testSessionPair struct {
	roomTag        [16]byte
	firstMaterial  SessionMaterial
	secondMaterial SessionMaterial
}

func newTestSessionPair(t testing.TB) testSessionPair {
	t.Helper()
	firstIdentity, firstSigner, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secondIdentity, secondSigner, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	firstPrivate, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secondPrivate, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var roomTag [16]byte
	var admissionKey [32]byte
	var firstNonce, secondNonce [16]byte
	_, _ = rand.Read(roomTag[:])
	_, _ = rand.Read(admissionKey[:])
	_, _ = rand.Read(firstNonce[:])
	_, _ = rand.Read(secondNonce[:])
	var firstPublic, secondPublic [32]byte
	copy(firstPublic[:], firstPrivate.PublicKey().Bytes())
	copy(secondPublic[:], secondPrivate.PublicKey().Bytes())
	firstWire, err := MarshalHello(roomTag, admissionKey, firstSigner, firstNonce, firstPublic)
	if err != nil {
		t.Fatal(err)
	}
	secondWire, err := MarshalHello(roomTag, admissionKey, secondSigner, secondNonce, secondPublic)
	if err != nil {
		t.Fatal(err)
	}
	firstHello, err := ParseHello(firstWire, roomTag, admissionKey)
	if err != nil {
		t.Fatal(err)
	}
	secondHello, err := ParseHello(secondWire, roomTag, admissionKey)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstHello.IdentityKey, firstIdentity) || !bytes.Equal(secondHello.IdentityKey, secondIdentity) {
		t.Fatal("parsed identity does not match signer")
	}
	firstMaterial, err := DeriveSession(firstPrivate, firstHello, secondHello)
	if err != nil {
		t.Fatal(err)
	}
	secondMaterial, err := DeriveSession(secondPrivate, secondHello, firstHello)
	if err != nil {
		t.Fatal(err)
	}
	return testSessionPair{
		roomTag: roomTag, firstMaterial: firstMaterial, secondMaterial: secondMaterial,
	}
}

func TestSessionMaterialAndControlPacket(t *testing.T) {
	pair := newTestSessionPair(t)
	if pair.firstMaterial.SessionID != pair.secondMaterial.SessionID {
		t.Fatal("session ID is not symmetric")
	}
	packet, err := MarshalControl(PacketPing, pair.roomTag, pair.firstMaterial.SessionID, 7, 99, pair.firstMaterial.Ciphers.ControlSend)
	if err != nil {
		t.Fatal(err)
	}
	if len(packet) != controlPacketSize {
		t.Fatalf("control packet length = %d, want %d", len(packet), controlPacketSize)
	}
	header, err := ParseEstablishedHeader(packet)
	if err != nil || header.Sequence != 7 {
		t.Fatalf("control header = %#v, %v", header, err)
	}
	decoded, err := ParseControl(packet, pair.roomTag, pair.secondMaterial.SessionID, pair.secondMaterial.Ciphers.ControlRecv)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Type != PacketPing || decoded.Challenge != 99 {
		t.Fatalf("decoded packet = %#v", decoded)
	}
	response, err := MarshalControl(PacketPong, pair.roomTag, pair.secondMaterial.SessionID, 8, 100, pair.secondMaterial.Ciphers.ControlSend)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err = ParseControl(response, pair.roomTag, pair.firstMaterial.SessionID, pair.firstMaterial.Ciphers.ControlRecv)
	if err != nil || decoded.Type != PacketPong || decoded.Challenge != 100 {
		t.Fatalf("reverse decoded packet = %#v, %v", decoded, err)
	}
}

func TestControlRejectsWrongFlowAndTampering(t *testing.T) {
	pair := newTestSessionPair(t)
	packet, err := MarshalControl(PacketPong, pair.roomTag, pair.firstMaterial.SessionID, 1, 2, pair.firstMaterial.Ciphers.ControlSend)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseControl(append([]byte(nil), packet...), pair.roomTag, pair.firstMaterial.SessionID, pair.firstMaterial.Ciphers.ControlRecv); err == nil {
		t.Fatal("ParseControl() accepted the wrong directional control key")
	}
	packet[len(packet)-1] ^= 0xff
	if _, err := ParseControl(packet, pair.roomTag, pair.firstMaterial.SessionID, pair.secondMaterial.Ciphers.ControlRecv); err == nil {
		t.Fatal("ParseControl() accepted a tampered packet")
	}
	if _, err := MarshalControl(PacketGroupDatagram, pair.roomTag, pair.firstMaterial.SessionID, 1, 1, pair.firstMaterial.Ciphers.ControlSend); err == nil {
		t.Fatal("MarshalControl() accepted a group datagram packet type")
	}
}

func TestSessionMaterialBindsHandshakeNonce(t *testing.T) {
	_, localSigner, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, remoteSigner, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	localPrivate, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	remotePrivate, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var roomTag [16]byte
	var admissionKey [32]byte
	var localPublic, remotePublic [32]byte
	copy(localPublic[:], localPrivate.PublicKey().Bytes())
	copy(remotePublic[:], remotePrivate.PublicKey().Bytes())
	localWire, err := MarshalHello(roomTag, admissionKey, localSigner, [16]byte{1}, localPublic)
	if err != nil {
		t.Fatal(err)
	}
	localHello, err := ParseHello(localWire, roomTag, admissionKey)
	if err != nil {
		t.Fatal(err)
	}
	derive := func(nonce byte) SessionMaterial {
		wire, err := MarshalHello(roomTag, admissionKey, remoteSigner, [16]byte{nonce}, remotePublic)
		if err != nil {
			t.Fatal(err)
		}
		hello, err := ParseHello(wire, roomTag, admissionKey)
		if err != nil {
			t.Fatal(err)
		}
		material, err := DeriveSession(localPrivate, localHello, hello)
		if err != nil {
			t.Fatal(err)
		}
		return material
	}
	first := derive(2)
	second := derive(3)
	if first.SessionID == second.SessionID {
		t.Fatal("changing a signed hello nonce reused the session ID")
	}
}

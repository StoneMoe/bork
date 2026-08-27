package protocol

import (
	"crypto/ecdh"
	"crypto/rand"
	"testing"

	"bork/internal/identity"
)

func TestSessionHelloTranscriptDerivesMatchingCiphers(t *testing.T) {
	roomTag := [16]byte{1}
	admissionKey := [32]byte{2}
	handshakeID := [16]byte{3}
	firstIdentity, err := identity.New()
	if err != nil {
		t.Fatal(err)
	}
	secondIdentity, err := identity.New()
	if err != nil {
		t.Fatal(err)
	}
	firstPrivate, firstHello := makeTestSessionHello(t, firstIdentity, roomTag, admissionKey, handshakeID)
	secondPrivate, secondHello := makeTestSessionHello(t, secondIdentity, roomTag, admissionKey, handshakeID)

	firstMaterial, err := DeriveSession(firstPrivate, firstHello, secondHello)
	if err != nil {
		t.Fatal(err)
	}
	secondMaterial, err := DeriveSession(secondPrivate, secondHello, firstHello)
	if err != nil {
		t.Fatal(err)
	}
	if firstMaterial.SessionID != secondMaterial.SessionID {
		t.Fatal("peers derived different session IDs from the same transcript")
	}

	packet, err := MarshalControl(PacketPing, roomTag, firstMaterial.SessionID, 1, 42, firstMaterial.Ciphers.ControlSend)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := ParseControl(packet, roomTag, secondMaterial.SessionID, secondMaterial.Ciphers.ControlRecv)
	if err != nil || decoded.Challenge != 42 {
		t.Fatalf("peer could not decrypt the derived send cipher: decoded=%+v err=%v", decoded, err)
	}
}

func TestSessionDerivationRejectsProbeAndMismatchedHandshake(t *testing.T) {
	roomTag := [16]byte{1}
	admissionKey := [32]byte{2}
	firstIdentity, err := identity.New()
	if err != nil {
		t.Fatal(err)
	}
	secondIdentity, err := identity.New()
	if err != nil {
		t.Fatal(err)
	}
	firstPrivate, firstHello := makeTestSessionHello(t, firstIdentity, roomTag, admissionKey, [16]byte{3})
	_, secondHello := makeTestSessionHello(t, secondIdentity, roomTag, admissionKey, [16]byte{4})

	if _, err := DeriveSession(firstPrivate, firstHello, secondHello); err == nil {
		t.Fatal("session derivation accepted different handshake IDs")
	}
	probePacket, err := MarshalHelloProbe(roomTag, admissionKey, secondIdentity)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := ParseHello(probePacket, roomTag, admissionKey)
	if err != nil {
		t.Fatal(err)
	}
	if !probe.IsProbe() {
		t.Fatal("hello probe was parsed as a session hello")
	}
	if _, err := DeriveSession(firstPrivate, firstHello, probe); err == nil {
		t.Fatal("session derivation accepted a discovery probe")
	}
}

func makeTestSessionHello(t *testing.T, signer *identity.LocalIdentity, roomTag [16]byte, admissionKey [32]byte, handshakeID [16]byte) (*ecdh.PrivateKey, HelloPacket) {
	t.Helper()
	privateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var publicKey [32]byte
	copy(publicKey[:], privateKey.PublicKey().Bytes())
	packet, err := MarshalSessionHello(roomTag, admissionKey, signer, handshakeID, publicKey)
	if err != nil {
		t.Fatal(err)
	}
	hello, err := ParseHello(packet, roomTag, admissionKey)
	if err != nil {
		t.Fatal(err)
	}
	return privateKey, hello
}

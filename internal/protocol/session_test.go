package protocol

import (
	"crypto/ecdh"
	"crypto/rand"
	"testing"

	"bork/internal/identity"
)

func TestSessionHelloTranscriptDerivesMatchingCiphers(t *testing.T) {
	admissionKey := [32]byte{2}
	sessionID := [16]byte{3}
	firstID, err := identity.New()
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := identity.New()
	if err != nil {
		t.Fatal(err)
	}
	firstPrivate, firstHello := makeTestSessionHello(t, firstID, admissionKey, sessionID)
	secondPrivate, secondHello := makeTestSessionHello(t, secondID, admissionKey, sessionID)

	firstCiphers, err := DeriveSession(firstPrivate, firstHello, secondHello)
	if err != nil {
		t.Fatal(err)
	}
	secondCiphers, err := DeriveSession(secondPrivate, secondHello, firstHello)
	if err != nil {
		t.Fatal(err)
	}

	packet, err := MarshalControl(PacketPong, sessionID, 1, 42, firstCiphers.Send)
	if err != nil {
		t.Fatal(err)
	}
	pingSequence, err := ParseControl(packet, sessionID, secondCiphers.Receive)
	if err != nil || pingSequence != 42 {
		t.Fatalf("peer could not decrypt the derived send cipher: pingSequence=%d err=%v", pingSequence, err)
	}
}

func TestSessionDerivationRejectsMismatchedSession(t *testing.T) {
	admissionKey := [32]byte{2}
	firstID, err := identity.New()
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := identity.New()
	if err != nil {
		t.Fatal(err)
	}
	firstPrivate, firstHello := makeTestSessionHello(t, firstID, admissionKey, [16]byte{3})
	_, secondHello := makeTestSessionHello(t, secondID, admissionKey, [16]byte{4})

	if _, err := DeriveSession(firstPrivate, firstHello, secondHello); err == nil {
		t.Fatal("session derivation accepted different session IDs")
	}
}

func TestHelloProbeHasItsOwnWireType(t *testing.T) {
	admissionKey := [32]byte{2}
	peerID, err := identity.New()
	if err != nil {
		t.Fatal(err)
	}
	packet, err := MarshalHelloProbe(admissionKey, peerID)
	if err != nil {
		t.Fatal(err)
	}
	parsedPeerID, err := ParseHelloProbe(packet, admissionKey)
	if err != nil || parsedPeerID != peerID {
		t.Fatalf("hello probe did not round trip: peerID=%s err=%v", parsedPeerID, err)
	}
	if _, err := ParseSessionHello(packet, admissionKey); err == nil {
		t.Fatal("hello probe was parsed as a session hello")
	}
}

func makeTestSessionHello(t *testing.T, peerID identity.PeerID, admissionKey [32]byte, sessionID [16]byte) (*ecdh.PrivateKey, SessionHello) {
	t.Helper()
	privateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var publicKey [32]byte
	copy(publicKey[:], privateKey.PublicKey().Bytes())
	packet, err := MarshalSessionHello(admissionKey, peerID, sessionID, publicKey)
	if err != nil {
		t.Fatal(err)
	}
	hello, err := ParseSessionHello(packet, admissionKey)
	if err != nil {
		t.Fatal(err)
	}
	return privateKey, hello
}

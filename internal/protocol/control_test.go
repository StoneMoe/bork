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
	firstCiphers   LinkCiphers
	secondCiphers  LinkCiphers
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
	firstCiphers, err := NewLinkCiphers(firstMaterial.Keys)
	if err != nil {
		t.Fatal(err)
	}
	secondCiphers, err := NewLinkCiphers(secondMaterial.Keys)
	if err != nil {
		t.Fatal(err)
	}
	return testSessionPair{
		roomTag: roomTag, firstMaterial: firstMaterial, secondMaterial: secondMaterial,
		firstCiphers: firstCiphers, secondCiphers: secondCiphers,
	}
}

func TestSessionKeysAndControlPacket(t *testing.T) {
	pair := newTestSessionPair(t)
	if pair.firstMaterial.SessionID != pair.secondMaterial.SessionID || pair.firstMaterial.TranscriptHash != pair.secondMaterial.TranscriptHash {
		t.Fatal("session material is not symmetric")
	}
	if pair.firstMaterial.Keys.ControlSend != pair.secondMaterial.Keys.ControlRecv ||
		pair.firstMaterial.Keys.ControlRecv != pair.secondMaterial.Keys.ControlSend ||
		pair.firstMaterial.Keys.VoiceSend != pair.secondMaterial.Keys.VoiceRecv ||
		pair.firstMaterial.Keys.VoiceRecv != pair.secondMaterial.Keys.VoiceSend {
		t.Fatal("directional session keys do not match")
	}
	packet, err := MarshalControl(PacketPing, pair.roomTag, pair.firstMaterial.SessionID, 7, 99, pair.firstCiphers.ControlSend)
	if err != nil {
		t.Fatal(err)
	}
	if len(packet) != controlPacketSize {
		t.Fatalf("control packet length = %d, want %d", len(packet), controlPacketSize)
	}
	decoded, err := ParseControl(packet, pair.roomTag, pair.secondMaterial.SessionID, pair.secondCiphers.ControlRecv)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Type != PacketPing || decoded.Sequence != 7 || decoded.Challenge != 99 {
		t.Fatalf("decoded packet = %#v", decoded)
	}
}

func TestControlRejectsWrongFlowAndTampering(t *testing.T) {
	pair := newTestSessionPair(t)
	packet, err := MarshalControl(PacketPong, pair.roomTag, pair.firstMaterial.SessionID, 1, 2, pair.firstCiphers.ControlSend)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseControl(append([]byte(nil), packet...), pair.roomTag, pair.firstMaterial.SessionID, pair.secondCiphers.VoiceRecv); err == nil {
		t.Fatal("ParseControl() accepted the voice key")
	}
	packet[len(packet)-1] ^= 0xff
	if _, err := ParseControl(packet, pair.roomTag, pair.firstMaterial.SessionID, pair.secondCiphers.ControlRecv); err == nil {
		t.Fatal("ParseControl() accepted a tampered packet")
	}
	if _, err := MarshalControl(PacketVoice, pair.roomTag, pair.firstMaterial.SessionID, 1, 1, pair.firstCiphers.ControlSend); err == nil {
		t.Fatal("MarshalControl() accepted a voice packet type")
	}
}

func TestSessionKeysBindHandshakeNonce(t *testing.T) {
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
	if first.SessionID == second.SessionID || first.Keys == second.Keys {
		t.Fatal("changing a signed hello nonce reused session material")
	}
}

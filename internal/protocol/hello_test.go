package protocol

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

func TestHelloRoundTrip(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var roomTag [16]byte
	var admissionKey [32]byte
	var nonce [16]byte
	var ephemeralKey [32]byte
	_, _ = rand.Read(roomTag[:])
	_, _ = rand.Read(admissionKey[:])
	_, _ = rand.Read(nonce[:])
	_, _ = rand.Read(ephemeralKey[:])
	packet, err := MarshalHello(roomTag, admissionKey, privateKey, nonce, ephemeralKey)
	if err != nil {
		t.Fatal(err)
	}
	hello, err := ParseHello(packet, roomTag, admissionKey)
	if err != nil {
		t.Fatalf("ParseHello() error = %v", err)
	}
	if string(hello.IdentityKey) != string(publicKey) || hello.RoomTag != roomTag || hello.Nonce != nonce || hello.EphemeralKey != ephemeralKey {
		t.Fatalf("hello = %#v", hello)
	}
	packet[0] ^= 0xff
	if hello.wire[0] != Magic[0] {
		t.Fatal("parsed hello retained mutable packet storage")
	}
}

func TestHelloRejectsTampering(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var roomTag [16]byte
	var admissionKey [32]byte
	var nonce [16]byte
	nonce[0] = 1
	var ephemeralKey [32]byte
	ephemeralKey[0] = 1
	packet, err := MarshalHello(roomTag, admissionKey, privateKey, nonce, ephemeralKey)
	if err != nil {
		t.Fatal(err)
	}
	packet[prefixSize+3] ^= 0xff
	if _, err := ParseHello(packet, roomTag, admissionKey); err == nil {
		t.Fatal("ParseHello() error = nil for tampered packet")
	}
}

func TestHelloRejectsEmptyEphemeralKey(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MarshalHello([16]byte{}, [32]byte{}, privateKey, [16]byte{1}, [32]byte{}); err == nil {
		t.Fatal("MarshalHello() accepted an empty ephemeral key")
	}
}

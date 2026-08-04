package protocol

import (
	"bytes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
)

func TestGroupDatagramRoundTrip(t *testing.T) {
	roomTag := [16]byte{1}
	groupKey := [32]byte{2}
	signer, senderID := testGroupDatagramSigner(t)
	header := GroupDatagramHeader{Class: TrafficAudio, SenderID: senderID, StreamID: [16]byte{4}, Sequence: 7}
	protector := testGroupDatagramCipher(t, groupKey)
	packet, err := MarshalGroupDatagram(roomTag, header, 4242, []byte("opus-frame"), protector, signer)
	if err != nil {
		t.Fatal(err)
	}
	if !ValidPacketSize(PacketGroupDatagram, len(packet)) {
		t.Fatalf("packet size %d rejected", len(packet))
	}
	wantSize := groupDatagramHeaderSize + 4 + len("opus-frame") + aeadTagSize + ed25519.SignatureSize
	if len(packet) != wantSize {
		t.Fatalf("packet size = %d, want %d", len(packet), wantSize)
	}
	parsedHeader, err := ParseGroupDatagramHeader(packet, roomTag)
	if err != nil || parsedHeader != header {
		t.Fatalf("header = %#v, error = %v", parsedHeader, err)
	}
	decoded, err := ParseGroupDatagram(packet, roomTag, header, protector)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Timestamp != 4242 || !bytes.Equal(decoded.Payload, []byte("opus-frame")) {
		t.Fatalf("decoded = %#v", decoded)
	}
}

func TestGroupDatagramUnifiedKeyUsesStreamSequenceNonce(t *testing.T) {
	roomTag := [16]byte{1}
	groupKey := [32]byte{2}
	signer, senderID := testGroupDatagramSigner(t)
	protector := testGroupDatagramCipher(t, groupKey)
	firstHeader := GroupDatagramHeader{Class: TrafficAudio, SenderID: senderID, StreamID: [16]byte{3}, Sequence: 1}
	secondHeader := firstHeader
	secondHeader.StreamID = [16]byte{4}
	first, err := MarshalGroupDatagram(roomTag, firstHeader, 10, []byte("same"), protector, signer)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MarshalGroupDatagram(roomTag, secondHeader, 10, []byte("same"), protector, signer)
	if err != nil {
		t.Fatal(err)
	}
	firstCiphertext := first[groupDatagramHeaderSize : len(first)-ed25519.SignatureSize]
	secondCiphertext := second[groupDatagramHeaderSize : len(second)-ed25519.SignatureSize]
	if bytes.Equal(firstCiphertext, secondCiphertext) {
		t.Fatal("different StreamIDs reused the same XChaCha nonce")
	}
}

func TestGroupDatagramRejectsWrongKeyAndTamper(t *testing.T) {
	roomTag := [16]byte{1}
	signer, senderID := testGroupDatagramSigner(t)
	header := GroupDatagramHeader{Class: TrafficInteractive, SenderID: senderID, StreamID: [16]byte{3}, Sequence: 1}
	protector := testGroupDatagramCipher(t, [32]byte{4})
	packet, err := MarshalGroupDatagram(roomTag, header, 9, []byte("frame"), protector, signer)
	if err != nil {
		t.Fatal(err)
	}
	wrong := testGroupDatagramCipher(t, [32]byte{5})
	if _, err := ParseGroupDatagram(bytes.Clone(packet), roomTag, header, wrong); err == nil || !strings.Contains(err.Error(), "authentication") {
		t.Fatalf("wrong key error = %v, want authentication failure", err)
	}
	packet[groupDatagramHeaderSize] ^= 1
	if _, err := ParseGroupDatagram(packet, roomTag, header, protector); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("tampered ciphertext error = %v, want signature failure", err)
	}
}

func TestParseGroupDatagramPreservesForwardableCiphertext(t *testing.T) {
	roomTag := [16]byte{1}
	signer, senderID := testGroupDatagramSigner(t)
	header := GroupDatagramHeader{Class: TrafficAudio, SenderID: senderID, StreamID: [16]byte{3}, Sequence: 4}
	protector := testGroupDatagramCipher(t, [32]byte{5})
	packet, err := MarshalGroupDatagram(roomTag, header, 6, []byte("frame"), protector, signer)
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Clone(packet)
	if _, err := ParseGroupDatagram(packet, roomTag, header, protector); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(packet, want) {
		t.Fatal("parsing mutated forwardable ciphertext")
	}
}

func TestGroupDatagramRejectsInvalidHeader(t *testing.T) {
	roomTag := [16]byte{1}
	groupKey := [32]byte{2}
	signer, senderID := testGroupDatagramSigner(t)
	valid := GroupDatagramHeader{Class: TrafficAudio, SenderID: senderID, StreamID: [16]byte{4}, Sequence: 1}
	protector := testGroupDatagramCipher(t, groupKey)
	tests := []GroupDatagramHeader{
		{Class: 0, SenderID: valid.SenderID, StreamID: valid.StreamID, Sequence: 1},
		{Class: TrafficAudio, StreamID: valid.StreamID, Sequence: 1},
		{Class: TrafficAudio, SenderID: valid.SenderID, Sequence: 1},
		{Class: TrafficAudio, SenderID: valid.SenderID, StreamID: valid.StreamID},
	}
	for _, header := range tests {
		if _, err := MarshalGroupDatagram(roomTag, header, 0, []byte{1}, protector, signer); err == nil {
			t.Fatalf("invalid header accepted: %#v", header)
		}
	}
}

func TestGroupDatagramRejectsForgedSenderIdentity(t *testing.T) {
	roomTag := [16]byte{1}
	groupKey := [32]byte{2}
	senderSigner, senderID := testGroupDatagramSigner(t)
	otherSigner, _ := testGroupDatagramSigner(t)
	header := GroupDatagramHeader{Class: TrafficAudio, SenderID: senderID, StreamID: [16]byte{3}, Sequence: 1}
	protector := testGroupDatagramCipher(t, groupKey)

	if _, err := MarshalGroupDatagram(roomTag, header, 4, []byte("frame"), protector, otherSigner); err == nil {
		t.Fatal("member signer was accepted for another sender identity")
	}
	packet, err := MarshalGroupDatagram(roomTag, header, 4, []byte("frame"), protector, senderSigner)
	if err != nil {
		t.Fatal(err)
	}
	signatureOffset := len(packet) - ed25519.SignatureSize
	copy(packet[signatureOffset:], ed25519.Sign(otherSigner, packet[:signatureOffset]))
	if _, err := ParseGroupDatagram(packet, roomTag, header, protector); err == nil {
		t.Fatal("signature from another room member was accepted for the claimed sender")
	}
}

func testGroupDatagramCipher(t testing.TB, groupKey [32]byte) cipher.AEAD {
	t.Helper()
	return NewGroupDatagramCipher(groupKey)
}

func testGroupDatagramSigner(t testing.TB) (ed25519.PrivateKey, [32]byte) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var senderID [32]byte
	copy(senderID[:], publicKey)
	return privateKey, senderID
}

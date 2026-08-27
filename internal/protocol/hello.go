package protocol

import (
	"crypto"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"errors"

	"bork/internal/identity"
)

// HelloPacket is either a discovery probe with empty handshake fields or one
// side of a Session transcript with both handshake fields populated.
type HelloPacket struct {
	RoomTag      [16]byte
	HandshakeID  [16]byte
	PeerID       identity.PeerID
	EphemeralKey [32]byte
	wire         [helloPacketSize]byte
}

// MarshalHelloProbe creates an authenticated discovery probe. A probe identifies
// its sender but is never part of a Session transcript.
func MarshalHelloProbe(roomTag [16]byte, admissionKey [32]byte, signer crypto.Signer) ([]byte, error) {
	return marshalHello(roomTag, admissionKey, signer, [16]byte{}, [32]byte{})
}

// MarshalSessionHello creates one side of a Session transcript. Both peers use
// the same handshake ID and keep this packet for the lifetime of that Session.
func MarshalSessionHello(roomTag [16]byte, admissionKey [32]byte, signer crypto.Signer, handshakeID [16]byte, ephemeralKey [32]byte) ([]byte, error) {
	if handshakeID == [16]byte{} {
		return nil, errors.New("session hello handshake ID is empty")
	}
	if ephemeralKey == [32]byte{} {
		return nil, errors.New("session hello ephemeral key is empty")
	}
	return marshalHello(roomTag, admissionKey, signer, handshakeID, ephemeralKey)
}

func marshalHello(roomTag [16]byte, admissionKey [32]byte, signer crypto.Signer, handshakeID [16]byte, ephemeralKey [32]byte) ([]byte, error) {
	publicKey, ok := signer.Public().(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		return nil, errors.New("hello signer must use Ed25519")
	}
	packet := make([]byte, 0, helloPacketSize)
	packet = appendPrefix(packet, PacketHello, roomTag)
	packet = append(packet, handshakeID[:]...)
	packet = append(packet, publicKey...)
	packet = append(packet, ephemeralKey[:]...)

	mac := hmac.New(sha256.New, admissionKey[:])
	_, _ = mac.Write(packet)
	packet = mac.Sum(packet)
	signature, err := signer.Sign(nil, packet, crypto.Hash(0))
	if err != nil {
		return nil, err
	}
	packet = append(packet, signature...)
	return packet, nil
}

func ParseHello(packet []byte, expectedRoomTag [16]byte, admissionKey [32]byte) (HelloPacket, error) {
	if len(packet) != helloPacketSize {
		return HelloPacket{}, errors.New("hello packet length is invalid")
	}
	packetType, roomTag, err := ParsePrefix(packet)
	if err != nil || packetType != PacketHello || roomTag != expectedRoomTag {
		return HelloPacket{}, errors.New("hello packet prefix is invalid")
	}
	bodyEnd := prefixSize + helloBodySize
	macEnd := bodyEnd + helloMACSize
	mac := hmac.New(sha256.New, admissionKey[:])
	_, _ = mac.Write(packet[:bodyEnd])
	if !hmac.Equal(packet[bodyEnd:macEnd], mac.Sum(nil)) {
		return HelloPacket{}, errors.New("hello admission MAC is invalid")
	}

	identityOffset := prefixSize + 16
	publicKey := ed25519.PublicKey(packet[identityOffset : identityOffset+ed25519.PublicKeySize])
	if !ed25519.Verify(publicKey, packet[:macEnd], packet[macEnd:]) {
		return HelloPacket{}, errors.New("hello peer ID signature is invalid")
	}

	var hello HelloPacket
	hello.RoomTag = roomTag
	copy(hello.HandshakeID[:], packet[prefixSize:prefixSize+16])
	copy(hello.PeerID[:], publicKey)
	copy(hello.EphemeralKey[:], packet[identityOffset+ed25519.PublicKeySize:bodyEnd])
	if (hello.HandshakeID == [16]byte{}) != (hello.EphemeralKey == [32]byte{}) {
		return HelloPacket{}, errors.New("hello handshake fields are inconsistent")
	}
	copy(hello.wire[:], packet)
	return hello, nil
}

func (hello HelloPacket) IsProbe() bool {
	return hello.HandshakeID == [16]byte{}
}

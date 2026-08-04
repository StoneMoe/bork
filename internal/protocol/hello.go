package protocol

import (
	"crypto"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
)

type HelloPacket struct {
	RoomTag      [16]byte
	IdentityKey  ed25519.PublicKey
	EphemeralKey [32]byte
	wire         [helloPacketSize]byte
}

func MarshalHello(roomTag [16]byte, admissionKey [32]byte, signer crypto.Signer, nonce [16]byte, ephemeralKey [32]byte) ([]byte, error) {
	publicKey, ok := signer.Public().(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		return nil, errors.New("hello signer must use Ed25519")
	}
	if nonce == [16]byte{} {
		return nil, errors.New("hello nonce is empty")
	}
	if ephemeralKey == [32]byte{} {
		return nil, errors.New("hello ephemeral key is empty")
	}
	packet := make([]byte, 0, helloPacketSize)
	packet = appendPrefix(packet, PacketHello, roomTag)
	packet = append(packet, nonce[:]...)
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
	publicKey := append(ed25519.PublicKey(nil), packet[identityOffset:identityOffset+ed25519.PublicKeySize]...)
	if !ed25519.Verify(publicKey, packet[:macEnd], packet[macEnd:]) {
		return HelloPacket{}, errors.New("hello identity signature is invalid")
	}

	var hello HelloPacket
	hello.RoomTag = roomTag
	if [16]byte(packet[prefixSize:prefixSize+16]) == [16]byte{} {
		return HelloPacket{}, errors.New("hello nonce is empty")
	}
	hello.IdentityKey = publicKey
	copy(hello.EphemeralKey[:], packet[identityOffset+ed25519.PublicKeySize:bodyEnd])
	if hello.EphemeralKey == [32]byte{} {
		return HelloPacket{}, errors.New("hello ephemeral key is empty")
	}
	copy(hello.wire[:], packet)
	return hello, nil
}

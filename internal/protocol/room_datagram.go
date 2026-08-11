package protocol

import (
	"bytes"
	"crypto"
	"crypto/cipher"
	"crypto/ed25519"
	"encoding/binary"
	"errors"

	"golang.org/x/crypto/chacha20poly1305"
)

type TrafficClass byte

const (
	TrafficAudio TrafficClass = iota + 1
	TrafficInteractive
)

type RoomDatagramHeader struct {
	Class    TrafficClass
	SenderID [32]byte
	StreamID [16]byte
	Sequence uint64
}

type RoomDatagram struct {
	Timestamp uint32
	Payload   []byte
}

func validTrafficClass(class TrafficClass) bool {
	return class >= TrafficAudio && class <= TrafficInteractive
}

func NewRoomDatagramCipher(roomDatagramKey [32]byte) cipher.AEAD {
	protector, _ := chacha20poly1305.NewX(roomDatagramKey[:])
	return protector
}

func ParseRoomDatagramHeader(packet []byte, expectedRoomTag [16]byte) (RoomDatagramHeader, error) {
	if len(packet) < roomDatagramHeaderSize {
		return RoomDatagramHeader{}, errors.New("room datagram is truncated")
	}
	packetType, roomTag, err := ParsePrefix(packet)
	if err != nil || packetType != PacketRoomDatagram || roomTag != expectedRoomTag {
		return RoomDatagramHeader{}, errors.New("room datagram prefix is invalid")
	}
	header := RoomDatagramHeader{Class: TrafficClass(packet[prefixSize])}
	copy(header.SenderID[:], packet[prefixSize+1:prefixSize+1+32])
	copy(header.StreamID[:], packet[prefixSize+1+32:prefixSize+1+32+16])
	header.Sequence = binary.BigEndian.Uint64(packet[roomDatagramHeaderSize-8 : roomDatagramHeaderSize])
	if !validTrafficClass(header.Class) || header.SenderID == ([32]byte{}) || header.StreamID == ([16]byte{}) || header.Sequence == 0 {
		return RoomDatagramHeader{}, errors.New("room datagram header is invalid")
	}
	return header, nil
}

func roomDatagramNonce(packet []byte) []byte {
	return packet[roomDatagramHeaderSize-chacha20poly1305.NonceSizeX : roomDatagramHeaderSize]
}

func MarshalRoomDatagram(roomTag [16]byte, header RoomDatagramHeader, timestamp uint32, payload []byte, protector cipher.AEAD, signer crypto.Signer) ([]byte, error) {
	if !validTrafficClass(header.Class) || header.SenderID == ([32]byte{}) || header.StreamID == ([16]byte{}) || header.Sequence == 0 {
		return nil, errors.New("room datagram header is invalid")
	}
	if len(payload) == 0 || len(payload) > MaxRoomDatagramPayload {
		return nil, errors.New("room datagram payload length is invalid")
	}
	if protector == nil || protector.NonceSize() != chacha20poly1305.NonceSizeX || protector.Overhead() != aeadTagSize {
		return nil, errors.New("room datagram protector is invalid")
	}
	if signer == nil {
		return nil, errors.New("room datagram signer is invalid")
	}
	publicKey, ok := signer.Public().(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize || !bytes.Equal(publicKey, header.SenderID[:]) {
		return nil, errors.New("room datagram signer does not match sender identity")
	}
	packet := make([]byte, 0, roomDatagramHeaderSize+4+len(payload)+aeadTagSize+roomDatagramSignatureSize)
	packet = appendPrefix(packet, PacketRoomDatagram, roomTag)
	packet = append(packet, byte(header.Class))
	packet = append(packet, header.SenderID[:]...)
	packet = append(packet, header.StreamID[:]...)
	packet = appendUint64(packet, header.Sequence)
	packet = binary.BigEndian.AppendUint32(packet, timestamp)
	packet = append(packet, payload...)
	body := packet[roomDatagramHeaderSize:]
	sealed := protector.Seal(body[:0], roomDatagramNonce(packet), body, packet[:roomDatagramHeaderSize])
	packet = packet[:roomDatagramHeaderSize+len(sealed)]
	signature, err := signer.Sign(nil, packet, crypto.Hash(0))
	if err != nil {
		return nil, errors.New("room datagram signing failed")
	}
	if len(signature) != roomDatagramSignatureSize {
		return nil, errors.New("room datagram signature length is invalid")
	}
	return append(packet, signature...), nil
}

func ParseRoomDatagram(packet []byte, expectedRoomTag [16]byte, expected RoomDatagramHeader, protector cipher.AEAD) (RoomDatagram, error) {
	if len(packet) < roomDatagramMinPacketSize || len(packet) > MaxDatagramSize {
		return RoomDatagram{}, errors.New("room datagram length is invalid")
	}
	if protector == nil || protector.NonceSize() != chacha20poly1305.NonceSizeX || protector.Overhead() != aeadTagSize {
		return RoomDatagram{}, errors.New("room datagram protector is invalid")
	}
	header, err := ParseRoomDatagramHeader(packet, expectedRoomTag)
	if err != nil || header != expected {
		return RoomDatagram{}, errors.New("room datagram header does not match")
	}
	signatureOffset := len(packet) - roomDatagramSignatureSize
	if !ed25519.Verify(ed25519.PublicKey(header.SenderID[:]), packet[:signatureOffset], packet[signatureOffset:]) {
		return RoomDatagram{}, errors.New("room datagram signature verification failed")
	}
	body := append([]byte(nil), packet[roomDatagramHeaderSize:signatureOffset]...)
	opened, err := protector.Open(body[:0], roomDatagramNonce(packet), body, packet[:roomDatagramHeaderSize])
	if err != nil || len(opened) <= 4 {
		return RoomDatagram{}, errors.New("room datagram authentication failed")
	}
	return RoomDatagram{
		Timestamp: binary.BigEndian.Uint32(opened[:4]),
		Payload:   opened[4:],
	}, nil
}

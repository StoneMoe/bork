package protocol

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"

	"golang.org/x/crypto/chacha20poly1305"
)

type RoomDatagramHeader struct {
	Type           PacketType
	StreamID       [16]byte
	PacketSequence uint64
}

type RoomDatagram struct {
	MediaUnitID uint32
	Payload     []byte
}

func validRoomDatagramType(packetType PacketType) bool {
	return packetType == PacketVoice || packetType == PacketScreenVideo || packetType == PacketScreenAudio
}

func NewRoomDatagramCipher(roomDatagramKey [32]byte) cipher.AEAD {
	protector, _ := chacha20poly1305.NewX(roomDatagramKey[:])
	return protector
}

func ParseRoomDatagramHeader(packet []byte) (RoomDatagramHeader, error) {
	if len(packet) < roomDatagramHeaderSize {
		return RoomDatagramHeader{}, errors.New("room datagram is truncated")
	}
	packetType, err := ParsePrefix(packet)
	if err != nil || !validRoomDatagramType(packetType) {
		return RoomDatagramHeader{}, errors.New("room datagram prefix is invalid")
	}
	header := RoomDatagramHeader{Type: packetType}
	copy(header.StreamID[:], packet[prefixSize:prefixSize+len(header.StreamID)])
	header.PacketSequence = binary.BigEndian.Uint64(packet[roomDatagramHeaderSize-8 : roomDatagramHeaderSize])
	if !validRoomDatagramHeader(header) {
		return RoomDatagramHeader{}, errors.New("room datagram header is invalid")
	}
	return header, nil
}

func roomDatagramNonce(packet []byte) []byte {
	return packet[roomDatagramHeaderSize-chacha20poly1305.NonceSizeX : roomDatagramHeaderSize]
}

func MarshalRoomDatagram(header RoomDatagramHeader, mediaUnitID uint32, payload []byte, protector cipher.AEAD) ([]byte, error) {
	if !validRoomDatagramHeader(header) {
		return nil, errors.New("room datagram header is invalid")
	}
	if !sizeWithin(len(payload), 1, MaxRoomDatagramPayload) {
		return nil, errors.New("room datagram payload length is invalid")
	}
	if !validRoomDatagramCipher(protector) {
		return nil, errors.New("room datagram protector is invalid")
	}
	packet := make([]byte, 0, roomDatagramHeaderSize+4+len(payload)+aeadTagSize)
	packet = appendPrefix(packet, header.Type)
	packet = append(packet, header.StreamID[:]...)
	packet = appendUint64(packet, header.PacketSequence)
	packet = binary.BigEndian.AppendUint32(packet, mediaUnitID)
	packet = append(packet, payload...)
	body := packet[roomDatagramHeaderSize:]
	sealed := protector.Seal(body[:0], roomDatagramNonce(packet), body, packet[:roomDatagramHeaderSize])
	return packet[:roomDatagramHeaderSize+len(sealed)], nil
}

func ParseRoomDatagram(packet []byte, expected RoomDatagramHeader, protector cipher.AEAD) (RoomDatagram, error) {
	if !sizeWithin(len(packet), roomDatagramMinPacketSize, MaxDatagramSize) {
		return RoomDatagram{}, errors.New("room datagram length is invalid")
	}
	if !validRoomDatagramCipher(protector) {
		return RoomDatagram{}, errors.New("room datagram protector is invalid")
	}
	header, err := ParseRoomDatagramHeader(packet)
	if err != nil || header != expected {
		return RoomDatagram{}, errors.New("room datagram header does not match")
	}
	body := packet[roomDatagramHeaderSize:]
	// Forwarders resend the original datagram after inspection, so decryption
	// must not overwrite its ciphertext in place.
	opened, err := protector.Open(nil, roomDatagramNonce(packet), body, packet[:roomDatagramHeaderSize])
	if err != nil || len(opened) <= 4 {
		return RoomDatagram{}, errors.New("room datagram authentication failed")
	}
	return RoomDatagram{
		MediaUnitID: binary.BigEndian.Uint32(opened[:4]),
		Payload:     opened[4:],
	}, nil
}

func validRoomDatagramHeader(header RoomDatagramHeader) bool {
	return validRoomDatagramType(header.Type) && header.StreamID != ([16]byte{}) && header.PacketSequence != 0
}

func validRoomDatagramCipher(protector cipher.AEAD) bool {
	return protector != nil && protector.NonceSize() == chacha20poly1305.NonceSizeX && protector.Overhead() == aeadTagSize
}

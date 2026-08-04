package protocol

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
)

type ControlPacket struct {
	Type      PacketType
	Challenge uint64
}

func MarshalControl(packetType PacketType, roomTag, sessionID [16]byte, sequence, challenge uint64, protector cipher.AEAD) ([]byte, error) {
	if packetType != PacketPing && packetType != PacketPong {
		return nil, errors.New("control packet type is invalid")
	}
	if sequence == 0 {
		return nil, errors.New("control packet sequence is zero")
	}
	if !validPairwiseCipher(protector) {
		return nil, errors.New("control packet protector is invalid")
	}
	packet := make([]byte, 0, controlPacketSize)
	packet = appendEstablishedHeader(packet, packetType, roomTag, sessionID, sequence)
	packet = appendUint64(packet, challenge)
	body := packet[establishedHeaderSize:]
	sealed := protector.Seal(body[:0], establishedNonce(packet), body, packet[:establishedHeaderSize])
	return packet[:establishedHeaderSize+len(sealed)], nil
}

func ParseControl(packet []byte, expectedRoomTag, expectedSessionID [16]byte, protector cipher.AEAD) (ControlPacket, error) {
	if len(packet) != controlPacketSize {
		return ControlPacket{}, errors.New("control packet length is invalid")
	}
	if !validPairwiseCipher(protector) {
		return ControlPacket{}, errors.New("control packet protector is invalid")
	}
	header, err := ParseEstablishedHeader(packet)
	if err != nil || (header.Type != PacketPing && header.Type != PacketPong) || header.RoomTag != expectedRoomTag {
		return ControlPacket{}, errors.New("control packet header is invalid")
	}
	if header.SessionID != expectedSessionID {
		return ControlPacket{}, errors.New("control packet session ID does not match")
	}
	body := packet[establishedHeaderSize:]
	opened, err := protector.Open(body[:0], establishedNonce(packet), body, packet[:establishedHeaderSize])
	if err != nil || len(opened) != controlPlaintextSize {
		return ControlPacket{}, errors.New("control packet authentication failed")
	}
	return ControlPacket{
		Type:      header.Type,
		Challenge: binary.BigEndian.Uint64(opened),
	}, nil
}

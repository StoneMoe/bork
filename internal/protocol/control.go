package protocol

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
)

// MarshalControl writes an empty Ping or Leave body. A Pong carries the
// PacketSequence of the Ping it answers.
func MarshalControl(packetType PacketType, sessionID [16]byte, packetSequence, pingSequence uint64, protector cipher.AEAD) ([]byte, error) {
	if sessionID == ([16]byte{}) || packetSequence == 0 || !validControlFields(packetType, pingSequence) {
		return nil, errors.New("control packet fields are invalid")
	}
	if !validPairwiseCipher(protector) {
		return nil, errors.New("control packet protector is invalid")
	}
	packet := make([]byte, 0, pongPacketSize)
	packet = appendSessionHeader(packet, packetType, sessionID, packetSequence)
	if packetType == PacketPong {
		packet = appendUint64(packet, pingSequence)
	}
	body := packet[sessionHeaderSize:]
	sealed := protector.Seal(body[:0], sessionNonce(packet), body, packet[:sessionHeaderSize])
	return packet[:sessionHeaderSize+len(sealed)], nil
}

func ParseControl(packet []byte, expectedSessionID [16]byte, protector cipher.AEAD) (uint64, error) {
	header, err := ParseSessionHeader(packet)
	if err != nil || !validControlPacket(header, expectedSessionID, len(packet)) {
		return 0, errors.New("control packet header is invalid")
	}
	if !validPairwiseCipher(protector) {
		return 0, errors.New("control packet protector is invalid")
	}
	body := packet[sessionHeaderSize:]
	opened, err := protector.Open(body[:0], sessionNonce(packet), body, packet[:sessionHeaderSize])
	if err != nil {
		return 0, errors.New("control packet authentication failed")
	}
	return decodeControlBody(header.Type, opened)
}

func validControlPacket(header SessionHeader, sessionID [16]byte, size int) bool {
	return validControlType(header.Type) && header.SessionID == sessionID && ValidPacketSize(header.Type, size)
}

func validControlFields(packetType PacketType, pingSequence uint64) bool {
	if !validControlType(packetType) {
		return false
	}
	return (packetType == PacketPong) == (pingSequence != 0)
}

func validControlType(packetType PacketType) bool {
	switch packetType {
	case PacketPing, PacketPong, PacketLeave:
		return true
	default:
		return false
	}
}

func decodeControlBody(packetType PacketType, body []byte) (uint64, error) {
	if packetType != PacketPong {
		if len(body) != 0 {
			return 0, errors.New("control packet body is not empty")
		}
		return 0, nil
	}
	if len(body) != pongPlaintextSize {
		return 0, errors.New("pong packet body is invalid")
	}
	pingSequence := binary.BigEndian.Uint64(body)
	if pingSequence == 0 {
		return 0, errors.New("pong packet ping sequence is zero")
	}
	return pingSequence, nil
}

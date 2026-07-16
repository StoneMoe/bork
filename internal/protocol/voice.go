package protocol

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
)

type VoicePacket struct {
	SessionID [16]byte
	Sequence  uint64
	Timestamp uint32
	Payload   []byte
}

func MarshalVoice(roomTag, sessionID [16]byte, sequence uint64, timestamp uint32, payload []byte, protector cipher.AEAD) ([]byte, error) {
	if len(payload) == 0 || len(payload) > MaxVoicePayload {
		return nil, errors.New("voice payload length is invalid")
	}
	if sequence == 0 {
		return nil, errors.New("voice packet sequence is zero")
	}
	if protector == nil || protector.NonceSize() != 12 || protector.Overhead() != aeadTagSize {
		return nil, errors.New("voice packet protector is invalid")
	}
	packet := make([]byte, 0, establishedHeaderSize+voicePlaintextFixedSize+len(payload)+aeadTagSize)
	packet = appendEstablishedHeader(packet, PacketVoice, roomTag, sessionID, sequence)
	var encodedTimestamp [4]byte
	binary.BigEndian.PutUint32(encodedTimestamp[:], timestamp)
	packet = append(packet, encodedTimestamp[:]...)
	packet = append(packet, payload...)
	body := packet[establishedHeaderSize:]
	sealed := protector.Seal(body[:0], establishedNonce(packet), body, packet[:establishedHeaderSize])
	return packet[:establishedHeaderSize+len(sealed)], nil
}

func ParseVoice(packet []byte, expectedRoomTag, expectedSessionID [16]byte, protector cipher.AEAD) (VoicePacket, error) {
	minimumSize := establishedHeaderSize + voicePlaintextFixedSize + 1 + aeadTagSize
	if len(packet) < minimumSize || len(packet) > MaxVoicePacketSize {
		return VoicePacket{}, errors.New("voice packet length is invalid")
	}
	if protector == nil || protector.NonceSize() != 12 || protector.Overhead() != aeadTagSize {
		return VoicePacket{}, errors.New("voice packet protector is invalid")
	}
	header, err := ParseEstablishedHeader(packet)
	if err != nil || header.Type != PacketVoice || header.RoomTag != expectedRoomTag {
		return VoicePacket{}, errors.New("voice packet header is invalid")
	}
	if header.SessionID != expectedSessionID {
		return VoicePacket{}, errors.New("voice packet session ID does not match")
	}
	body := packet[establishedHeaderSize:]
	opened, err := protector.Open(body[:0], establishedNonce(packet), body, packet[:establishedHeaderSize])
	if err != nil || len(opened) <= voicePlaintextFixedSize {
		return VoicePacket{}, errors.New("voice packet authentication failed")
	}
	return VoicePacket{
		SessionID: header.SessionID,
		Sequence:  header.Sequence,
		Timestamp: binary.BigEndian.Uint32(opened[:4]),
		Payload:   opened[4:],
	}, nil
}

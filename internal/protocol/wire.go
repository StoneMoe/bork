package protocol

import (
	"encoding/binary"
	"errors"
)

const (
	Version    byte = 1
	wireDomain      = "bork/wire-v1/"

	PacketHello PacketType = 1
	PacketPing  PacketType = 2
	PacketPong  PacketType = 3
	PacketVoice PacketType = 4

	prefixSize            = 4 + 1 + 1 + 16
	establishedHeaderSize = prefixSize + 16 + 8
	aeadTagSize           = 16

	helloBodySize   = 16 + 32 + 32
	helloMACSize    = 32
	helloSigSize    = 64
	helloPacketSize = prefixSize + helloBodySize + helloMACSize + helloSigSize

	controlPlaintextSize = 8
	controlPacketSize    = establishedHeaderSize + controlPlaintextSize + aeadTagSize

	MaxVoicePacketSize      = 1200
	MaxVoicePayload         = MaxVoicePacketSize - establishedHeaderSize - voicePlaintextFixedSize - aeadTagSize
	voicePlaintextFixedSize = 4
)

type PacketType byte

var Magic = [4]byte{'B', 'R', 'K', '0' + Version}

func appendPrefix(destination []byte, packetType PacketType, roomTag [16]byte) []byte {
	destination = append(destination, Magic[:]...)
	destination = append(destination, Version, byte(packetType))
	destination = append(destination, roomTag[:]...)
	return destination
}

func ParsePrefix(packet []byte) (PacketType, [16]byte, error) {
	var roomTag [16]byte
	if len(packet) < prefixSize {
		return 0, roomTag, errors.New("Bork packet is truncated")
	}
	if string(packet[:4]) != string(Magic[:]) || packet[4] != Version {
		return 0, roomTag, errors.New("Bork packet magic or version is invalid")
	}
	copy(roomTag[:], packet[6:prefixSize])
	return PacketType(packet[5]), roomTag, nil
}

func ValidPacketSize(packetType PacketType, size int) bool {
	switch packetType {
	case PacketHello:
		return size == helloPacketSize
	case PacketPing, PacketPong:
		return size == controlPacketSize
	case PacketVoice:
		return size >= establishedHeaderSize+voicePlaintextFixedSize+1+aeadTagSize && size <= MaxVoicePacketSize
	default:
		return false
	}
}

type EstablishedHeader struct {
	Type      PacketType
	RoomTag   [16]byte
	SessionID [16]byte
	Sequence  uint64
}

func appendEstablishedHeader(destination []byte, packetType PacketType, roomTag, sessionID [16]byte, sequence uint64) []byte {
	destination = appendPrefix(destination, packetType, roomTag)
	destination = append(destination, sessionID[:]...)
	return appendUint64(destination, sequence)
}

func ParseEstablishedHeader(packet []byte) (EstablishedHeader, error) {
	if len(packet) < establishedHeaderSize {
		return EstablishedHeader{}, errors.New("established packet is truncated")
	}
	packetType, roomTag, err := ParsePrefix(packet)
	if err != nil || (packetType != PacketPing && packetType != PacketPong && packetType != PacketVoice) {
		return EstablishedHeader{}, errors.New("established packet prefix is invalid")
	}
	var header EstablishedHeader
	header.Type = packetType
	header.RoomTag = roomTag
	copy(header.SessionID[:], packet[prefixSize:prefixSize+16])
	header.Sequence = binary.BigEndian.Uint64(packet[prefixSize+16 : establishedHeaderSize])
	if header.Sequence == 0 {
		return EstablishedHeader{}, errors.New("established packet sequence is zero")
	}
	return header, nil
}

func establishedNonce(packet []byte) []byte {
	return packet[establishedHeaderSize-12 : establishedHeaderSize]
}

func appendUint64(destination []byte, value uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	return append(destination, encoded[:]...)
}

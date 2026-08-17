package protocol

import (
	"crypto/cipher"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
)

const (
	Version    byte = 1
	wireDomain      = "bork/wire-v1/"

	PacketHello         PacketType = 1
	PacketPing          PacketType = 2
	PacketPong          PacketType = 3
	PacketBridgeControl PacketType = 4
	PacketRoomDatagram  PacketType = 5
	PacketReliable      PacketType = 6
	PacketLeave         PacketType = 7

	prefixSize        = 4 + 1 + 16
	sessionHeaderSize = prefixSize + 16 + 8
	aeadTagSize       = 16

	helloBodySize   = 16 + 32 + 32
	helloMACSize    = 32
	helloSigSize    = 64
	helloPacketSize = prefixSize + helloBodySize + helloMACSize + helloSigSize

	controlPlaintextSize = 8
	controlPacketSize    = sessionHeaderSize + controlPlaintextSize + aeadTagSize

	MaxDatagramSize = 1200

	// Room datagrams are sealed once per stream and forwarded verbatim. The
	// authenticated cleartext header exposes only scheduling/routing metadata.
	roomDatagramHeaderSize    = prefixSize + 1 + 32 + 16 + 8
	roomDatagramSignatureSize = ed25519.SignatureSize
	MaxRoomDatagramPayload    = MaxDatagramSize - roomDatagramHeaderSize - 4 - aeadTagSize - roomDatagramSignatureSize
	roomDatagramMinPacketSize = roomDatagramHeaderSize + 4 + 1 + aeadTagSize + roomDatagramSignatureSize
)

type PacketType byte

var Magic = [4]byte{'B', 'R', 'K', '0' + Version}

func appendPrefix(destination []byte, packetType PacketType, roomTag [16]byte) []byte {
	destination = append(destination, Magic[:]...)
	destination = append(destination, byte(packetType))
	destination = append(destination, roomTag[:]...)
	return destination
}

func ParsePrefix(packet []byte) (PacketType, [16]byte, error) {
	var roomTag [16]byte
	if len(packet) < prefixSize {
		return 0, roomTag, errors.New("Bork packet is truncated")
	}
	if string(packet[:4]) != string(Magic[:]) {
		return 0, roomTag, errors.New("Bork packet magic is invalid")
	}
	copy(roomTag[:], packet[5:prefixSize])
	return PacketType(packet[4]), roomTag, nil
}

func ValidPacketSize(packetType PacketType, size int) bool {
	switch packetType {
	case PacketHello:
		return size == helloPacketSize
	case PacketPing, PacketPong, PacketLeave:
		return size == controlPacketSize
	case PacketBridgeControl:
		return size >= bridgeMinPacketSize && size <= MaxDatagramSize
	case PacketRoomDatagram:
		return size >= roomDatagramMinPacketSize && size <= MaxDatagramSize
	case PacketReliable:
		return size >= sessionHeaderSize+reliablePlaintextFixedSize+aeadTagSize && size <= MaxBridgeInnerSize
	default:
		return false
	}
}

type SessionHeader struct {
	Type      PacketType
	RoomTag   [16]byte
	SessionID [16]byte
	Sequence  uint64
}

func appendSessionHeader(destination []byte, packetType PacketType, roomTag, sessionID [16]byte, sequence uint64) []byte {
	destination = appendPrefix(destination, packetType, roomTag)
	destination = append(destination, sessionID[:]...)
	return appendUint64(destination, sequence)
}

func ParseSessionHeader(packet []byte) (SessionHeader, error) {
	if len(packet) < sessionHeaderSize {
		return SessionHeader{}, errors.New("session packet is truncated")
	}
	packetType, roomTag, err := ParsePrefix(packet)
	if err != nil || (packetType != PacketPing && packetType != PacketPong && packetType != PacketBridgeControl && packetType != PacketReliable && packetType != PacketLeave) {
		return SessionHeader{}, errors.New("session packet prefix is invalid")
	}
	var header SessionHeader
	header.Type = packetType
	header.RoomTag = roomTag
	copy(header.SessionID[:], packet[prefixSize:prefixSize+16])
	header.Sequence = binary.BigEndian.Uint64(packet[prefixSize+16 : sessionHeaderSize])
	if header.Sequence == 0 {
		return SessionHeader{}, errors.New("session packet sequence is zero")
	}
	return header, nil
}

func sessionNonce(packet []byte) []byte {
	return packet[sessionHeaderSize-12 : sessionHeaderSize]
}

func validPairwiseCipher(protector cipher.AEAD) bool {
	return protector != nil && protector.NonceSize() == 12 && protector.Overhead() == aeadTagSize
}

func appendUint64(destination []byte, value uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	return append(destination, encoded[:]...)
}

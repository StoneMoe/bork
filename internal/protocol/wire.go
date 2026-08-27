package protocol

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"

	"bork/internal/identity"
)

const (
	Version    byte = 3
	wireDomain      = "bork/wire-v3/"

	PacketHelloProbe        PacketType = 1
	PacketSessionHello      PacketType = 2
	PacketPing              PacketType = 3
	PacketPong              PacketType = 4
	PacketBridge            PacketType = 5
	PacketBridgeLowPriority PacketType = 6
	PacketVoice             PacketType = 7
	PacketScreenVideo       PacketType = 8
	PacketScreenAudio       PacketType = 9
	PacketReliable          PacketType = 10
	PacketLeave             PacketType = 11

	prefixSize        = 4 + 1
	sessionHeaderSize = prefixSize + 16 + 8
	aeadTagSize       = 16

	helloMACSize           = 32
	helloProbeBodySize     = len(identity.PeerID{})
	helloProbePacketSize   = prefixSize + helloProbeBodySize + helloMACSize
	sessionHelloBodySize   = 16 + len(identity.PeerID{}) + 32
	sessionHelloPacketSize = prefixSize + sessionHelloBodySize + helloMACSize

	emptyControlPacketSize = sessionHeaderSize + aeadTagSize
	pongPlaintextSize      = 8
	pongPacketSize         = sessionHeaderSize + pongPlaintextSize + aeadTagSize

	MaxDatagramSize = 1200

	// Room datagrams are sealed once per stream and forwarded verbatim. The
	// authenticated cleartext header exposes only scheduling metadata.
	roomDatagramHeaderSize    = prefixSize + 16 + 8
	MaxRoomDatagramPayload    = MaxDatagramSize - roomDatagramHeaderSize - 4 - aeadTagSize
	roomDatagramMinPacketSize = roomDatagramHeaderSize + 4 + 1 + aeadTagSize
)

type PacketType byte

var Magic = [4]byte{'B', 'R', 'K', '0' + Version}

var fixedPacketSizes = [...]int{
	PacketHelloProbe:   helloProbePacketSize,
	PacketSessionHello: sessionHelloPacketSize,
	PacketPing:         emptyControlPacketSize,
	PacketPong:         pongPacketSize,
	PacketLeave:        emptyControlPacketSize,
}

func appendPrefix(destination []byte, packetType PacketType) []byte {
	destination = append(destination, Magic[:]...)
	return append(destination, byte(packetType))
}

func ParsePrefix(packet []byte) (PacketType, error) {
	if len(packet) < prefixSize {
		return 0, errors.New("Bork packet is truncated")
	}
	if string(packet[:4]) != string(Magic[:]) {
		return 0, errors.New("Bork packet magic is invalid")
	}
	return PacketType(packet[4]), nil
}

func ValidPacketSize(packetType PacketType, size int) bool {
	if int(packetType) < len(fixedPacketSizes) {
		if expected := fixedPacketSizes[packetType]; expected != 0 {
			return size == expected
		}
	}
	switch packetType {
	case PacketBridge:
		return sizeWithin(size, bridgeMinPacketSize, MaxDatagramSize)
	case PacketBridgeLowPriority:
		return sizeWithin(size, bridgeLowPriorityMinPacketSize, MaxDatagramSize)
	case PacketVoice, PacketScreenVideo, PacketScreenAudio:
		return sizeWithin(size, roomDatagramMinPacketSize, MaxDatagramSize)
	case PacketReliable:
		return sizeWithin(size, reliableMinPacketSize, MaxBridgeInnerSize)
	default:
		return false
	}
}

func sizeWithin(size, minimum, maximum int) bool {
	return size >= minimum && size <= maximum
}

type SessionHeader struct {
	Type           PacketType
	SessionID      [16]byte
	PacketSequence uint64
}

func appendSessionHeader(destination []byte, packetType PacketType, sessionID [16]byte, sequence uint64) []byte {
	destination = appendPrefix(destination, packetType)
	destination = append(destination, sessionID[:]...)
	return appendUint64(destination, sequence)
}

func ParseSessionHeader(packet []byte) (SessionHeader, error) {
	if len(packet) < sessionHeaderSize {
		return SessionHeader{}, errors.New("session packet is truncated")
	}
	packetType, err := ParsePrefix(packet)
	if err != nil || !validSessionPacketType(packetType) {
		return SessionHeader{}, errors.New("session packet prefix is invalid")
	}
	var header SessionHeader
	header.Type = packetType
	copy(header.SessionID[:], packet[prefixSize:prefixSize+16])
	header.PacketSequence = binary.BigEndian.Uint64(packet[prefixSize+16 : sessionHeaderSize])
	if header.SessionID == ([16]byte{}) || header.PacketSequence == 0 {
		return SessionHeader{}, errors.New("session packet header is invalid")
	}
	return header, nil
}

func validSessionPacketType(packetType PacketType) bool {
	switch packetType {
	case PacketPing, PacketPong, PacketBridge, PacketBridgeLowPriority, PacketReliable, PacketLeave:
		return true
	default:
		return false
	}
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

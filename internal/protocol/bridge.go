package protocol

import (
	"crypto/cipher"
	"errors"

	"bork/internal/identity"
)

const (
	bridgeBodyFixedSize            = len(identity.PeerID{})
	MaxBridgeInnerSize             = MaxDatagramSize - sessionHeaderSize - aeadTagSize - bridgeBodyFixedSize
	bridgeMinPacketSize            = sessionHeaderSize + aeadTagSize + bridgeBodyFixedSize + emptyControlPacketSize
	bridgeLowPriorityMinPacketSize = sessionHeaderSize + aeadTagSize + bridgeBodyFixedSize + reliableMinPacketSize
)

type BridgePacket struct {
	Target      identity.PeerID
	LowPriority bool
	Inner       []byte
}

func MarshalBridge(sessionID [16]byte, packetSequence uint64, target identity.PeerID, lowPriority bool, inner []byte, protector cipher.AEAD) ([]byte, error) {
	if !validBridgeFields(sessionID, packetSequence, target, len(inner)) {
		return nil, errors.New("bridge packet fields are invalid")
	}
	if !validPairwiseCipher(protector) {
		return nil, errors.New("bridge packet protector is invalid")
	}
	if !validBridgeInner(inner, lowPriority) {
		return nil, errors.New("bridge inner packet is invalid")
	}

	packetType := PacketBridge
	if lowPriority {
		packetType = PacketBridgeLowPriority
	}
	packet := make([]byte, 0, sessionHeaderSize+bridgeBodyFixedSize+len(inner)+aeadTagSize)
	packet = appendSessionHeader(packet, packetType, sessionID, packetSequence)
	packet = append(packet, target[:]...)
	packet = append(packet, inner...)
	body := packet[sessionHeaderSize:]
	sealed := protector.Seal(body[:0], sessionNonce(packet), body, packet[:sessionHeaderSize])
	return packet[:sessionHeaderSize+len(sealed)], nil
}

func ParseBridge(packet []byte, expectedSessionID [16]byte, protector cipher.AEAD) (BridgePacket, error) {
	if !validPairwiseCipher(protector) {
		return BridgePacket{}, errors.New("bridge packet protector is invalid")
	}
	header, err := ParseSessionHeader(packet)
	if err != nil {
		return BridgePacket{}, errors.New("bridge packet header is invalid")
	}
	lowPriority := header.Type == PacketBridgeLowPriority
	if header.SessionID != expectedSessionID || (header.Type != PacketBridge && !lowPriority) || !ValidPacketSize(header.Type, len(packet)) {
		return BridgePacket{}, errors.New("bridge packet header is invalid")
	}
	body := packet[sessionHeaderSize:]
	opened, err := protector.Open(body[:0], sessionNonce(packet), body, packet[:sessionHeaderSize])
	if err != nil {
		return BridgePacket{}, errors.New("bridge packet authentication failed")
	}
	return parseBridgeBody(opened, lowPriority)
}

func parseBridgeBody(opened []byte, lowPriority bool) (BridgePacket, error) {
	if len(opened) < bridgeBodyFixedSize {
		return BridgePacket{}, errors.New("bridge packet body is invalid")
	}
	var decoded BridgePacket
	copy(decoded.Target[:], opened[:len(decoded.Target)])
	if decoded.Target.IsZero() {
		return BridgePacket{}, errors.New("bridge packet target is invalid")
	}
	decoded.LowPriority = lowPriority
	decoded.Inner = opened[bridgeBodyFixedSize:]
	if !validBridgeInner(decoded.Inner, decoded.LowPriority) {
		return BridgePacket{}, errors.New("bridge inner packet is invalid")
	}
	return decoded, nil
}

func validBridgeFields(sessionID [16]byte, packetSequence uint64, target identity.PeerID, innerSize int) bool {
	return sessionID != ([16]byte{}) && packetSequence != 0 && !target.IsZero() && innerSize > 0 && innerSize <= MaxBridgeInnerSize
}

func validBridgeInner(inner []byte, lowPriority bool) bool {
	packetType, err := ParsePrefix(inner)
	if err != nil || !ValidPacketSize(packetType, len(inner)) {
		return false
	}
	if lowPriority && packetType != PacketReliable {
		return false
	}
	switch packetType {
	case PacketHelloProbe, PacketSessionHello, PacketPing, PacketPong, PacketReliable, PacketLeave:
		return true
	default:
		return false
	}
}

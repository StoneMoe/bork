package protocol

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
)

const (
	bridgeBodyFixedSize = 32 + 32 + 2
	MaxBridgeInnerSize  = MaxDatagramSize - sessionHeaderSize - aeadTagSize - bridgeBodyFixedSize
	bridgeMinPacketSize = sessionHeaderSize + aeadTagSize + bridgeBodyFixedSize + controlPacketSize
)

type BridgePacket struct {
	Origin     [32]byte
	Target     [32]byte
	Background bool
	Inner      []byte
}

func MarshalBridge(roomTag, sessionID [16]byte, sequence uint64, origin, target [32]byte, background bool, inner []byte, protector cipher.AEAD) ([]byte, error) {
	if sequence == 0 || !validBridgeEndpoints(origin, target) || len(inner) == 0 || len(inner) > MaxBridgeInnerSize {
		return nil, errors.New("bridge packet fields are invalid")
	}
	if !validPairwiseCipher(protector) {
		return nil, errors.New("bridge packet protector is invalid")
	}
	if !validBridgeInner(inner, roomTag) {
		return nil, errors.New("bridge inner packet is invalid")
	}
	if packetType, _, _ := ParsePrefix(inner); background && packetType != PacketReliable {
		return nil, errors.New("only reliable bridge packets may use the background lane")
	}

	packet := make([]byte, 0, MaxDatagramSize)
	packet = appendSessionHeader(packet, PacketBridgeControl, roomTag, sessionID, sequence)
	packet = append(packet, origin[:]...)
	packet = append(packet, target[:]...)
	innerLength := uint16(len(inner))
	if background {
		innerLength |= 1 << 15
	}
	packet = binary.BigEndian.AppendUint16(packet, innerLength)
	packet = append(packet, inner...)
	body := packet[sessionHeaderSize:]
	sealed := protector.Seal(body[:0], sessionNonce(packet), body, packet[:sessionHeaderSize])
	return packet[:sessionHeaderSize+len(sealed)], nil
}

func ParseBridge(packet []byte, expectedRoomTag, expectedSessionID [16]byte, protector cipher.AEAD) (BridgePacket, error) {
	if len(packet) < bridgeMinPacketSize || len(packet) > MaxDatagramSize {
		return BridgePacket{}, errors.New("bridge packet length is invalid")
	}
	if !validPairwiseCipher(protector) {
		return BridgePacket{}, errors.New("bridge packet protector is invalid")
	}
	header, err := ParseSessionHeader(packet)
	if err != nil || header.Type != PacketBridgeControl || header.RoomTag != expectedRoomTag || header.SessionID != expectedSessionID {
		return BridgePacket{}, errors.New("bridge packet header is invalid")
	}
	body := packet[sessionHeaderSize:]
	opened, err := protector.Open(body[:0], sessionNonce(packet), body, packet[:sessionHeaderSize])
	if err != nil || len(opened) < bridgeBodyFixedSize {
		return BridgePacket{}, errors.New("bridge packet authentication failed")
	}

	var decoded BridgePacket
	copy(decoded.Origin[:], opened[:32])
	copy(decoded.Target[:], opened[32:64])
	if !validBridgeEndpoints(decoded.Origin, decoded.Target) {
		return BridgePacket{}, errors.New("bridge packet endpoints are invalid")
	}
	encodedLength := binary.BigEndian.Uint16(opened[64:66])
	decoded.Background = encodedLength&(1<<15) != 0
	innerLength := int(encodedLength &^ (1 << 15))
	if innerLength == 0 || innerLength > MaxBridgeInnerSize || len(opened) != bridgeBodyFixedSize+innerLength {
		return BridgePacket{}, errors.New("bridge inner packet length is invalid")
	}
	decoded.Inner = opened[bridgeBodyFixedSize:]
	if !validBridgeInner(decoded.Inner, expectedRoomTag) {
		return BridgePacket{}, errors.New("bridge inner packet is invalid")
	}
	if packetType, _, _ := ParsePrefix(decoded.Inner); decoded.Background && packetType != PacketReliable {
		return BridgePacket{}, errors.New("bridge packet background lane is invalid")
	}
	return decoded, nil
}

func validBridgeEndpoints(origin, target [32]byte) bool {
	return origin != ([32]byte{}) && target != ([32]byte{}) && origin != target
}

func validBridgeInner(inner []byte, roomTag [16]byte) bool {
	packetType, innerRoomTag, err := ParsePrefix(inner)
	if err != nil || innerRoomTag != roomTag || !ValidPacketSize(packetType, len(inner)) {
		return false
	}
	return packetType == PacketHello || packetType == PacketPing || packetType == PacketPong || packetType == PacketReliable || packetType == PacketLeave
}

package protocol

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
)

const (
	ReliableFlagOrdered byte = 1 << 0
	ReliableFlagAckOnly byte = 1 << 1

	MaxReliableFragments       = 1024
	reliablePlaintextFixedSize = 2 + 1 + 8 + 8 + 2 + 2 + 8 + 8
	MaxReliablePayload         = MaxBridgeInnerSize - establishedHeaderSize - reliablePlaintextFixedSize - aeadTagSize
)

type ReliablePacket struct {
	Channel          uint16
	Flags            byte
	FragmentSequence uint64
	MessageSequence  uint64
	FragmentIndex    uint16
	FragmentCount    uint16
	AckBase          uint64
	AckBitmap        uint64
	Payload          []byte
}

func (p ReliablePacket) Ordered() bool {
	return p.Flags&ReliableFlagOrdered != 0
}

func (p ReliablePacket) AckOnly() bool {
	return p.Flags&ReliableFlagAckOnly != 0
}

func AckContains(base, bitmap, sequence uint64) bool {
	if base == 0 || sequence == 0 || sequence > base {
		return false
	}
	delta := base - sequence
	return delta < 64 && bitmap&(uint64(1)<<delta) != 0
}

func MarshalReliable(roomTag, sessionID [16]byte, packetSequence uint64, p ReliablePacket, protector cipher.AEAD) ([]byte, error) {
	if packetSequence == 0 {
		return nil, errors.New("reliable packet sequence is zero")
	}
	if !validPairwiseCipher(protector) {
		return nil, errors.New("reliable packet protector is invalid")
	}
	if err := validateReliableBody(p); err != nil {
		return nil, err
	}

	packet := make([]byte, 0, establishedHeaderSize+reliablePlaintextFixedSize+len(p.Payload)+aeadTagSize)
	packet = appendEstablishedHeader(packet, PacketReliable, roomTag, sessionID, packetSequence)
	var fixed [reliablePlaintextFixedSize]byte
	binary.BigEndian.PutUint16(fixed[0:2], p.Channel)
	fixed[2] = p.Flags
	binary.BigEndian.PutUint64(fixed[3:11], p.FragmentSequence)
	binary.BigEndian.PutUint64(fixed[11:19], p.MessageSequence)
	binary.BigEndian.PutUint16(fixed[19:21], p.FragmentIndex)
	binary.BigEndian.PutUint16(fixed[21:23], p.FragmentCount)
	binary.BigEndian.PutUint64(fixed[23:31], p.AckBase)
	binary.BigEndian.PutUint64(fixed[31:39], p.AckBitmap)
	packet = append(packet, fixed[:]...)
	packet = append(packet, p.Payload...)
	body := packet[establishedHeaderSize:]
	sealed := protector.Seal(body[:0], establishedNonce(packet), body, packet[:establishedHeaderSize])
	return packet[:establishedHeaderSize+len(sealed)], nil
}

func ParseReliable(packet []byte, expectedRoomTag, expectedSessionID [16]byte, protector cipher.AEAD) (ReliablePacket, error) {
	minimumSize := establishedHeaderSize + reliablePlaintextFixedSize + aeadTagSize
	if len(packet) < minimumSize || len(packet) > MaxBridgeInnerSize {
		return ReliablePacket{}, errors.New("reliable packet length is invalid")
	}
	if !validPairwiseCipher(protector) {
		return ReliablePacket{}, errors.New("reliable packet protector is invalid")
	}
	header, err := ParseEstablishedHeader(packet)
	if err != nil || header.Type != PacketReliable || header.RoomTag != expectedRoomTag || header.SessionID != expectedSessionID {
		return ReliablePacket{}, errors.New("reliable packet header is invalid")
	}
	body := packet[establishedHeaderSize:]
	opened, err := protector.Open(body[:0], establishedNonce(packet), body, packet[:establishedHeaderSize])
	if err != nil || len(opened) < reliablePlaintextFixedSize {
		return ReliablePacket{}, errors.New("reliable packet authentication failed")
	}

	decoded := ReliablePacket{
		Channel:          binary.BigEndian.Uint16(opened[0:2]),
		Flags:            opened[2],
		FragmentSequence: binary.BigEndian.Uint64(opened[3:11]),
		MessageSequence:  binary.BigEndian.Uint64(opened[11:19]),
		FragmentIndex:    binary.BigEndian.Uint16(opened[19:21]),
		FragmentCount:    binary.BigEndian.Uint16(opened[21:23]),
		AckBase:          binary.BigEndian.Uint64(opened[23:31]),
		AckBitmap:        binary.BigEndian.Uint64(opened[31:39]),
		Payload:          opened[reliablePlaintextFixedSize:],
	}
	if len(decoded.Payload) == 0 {
		decoded.Payload = nil
	}
	if err := validateReliableBody(decoded); err != nil {
		return ReliablePacket{}, err
	}
	return decoded, nil
}

func validateReliableBody(p ReliablePacket) error {
	if p.Channel == 0 {
		return errors.New("reliable packet channel is zero")
	}
	if p.Flags&^(ReliableFlagOrdered|ReliableFlagAckOnly) != 0 {
		return errors.New("reliable packet flags are invalid")
	}
	if !validReliableAck(p.AckBase, p.AckBitmap) {
		return errors.New("reliable packet acknowledgment is invalid")
	}
	if p.AckOnly() {
		if p.Flags != ReliableFlagAckOnly || p.FragmentSequence != 0 || p.MessageSequence != 0 || p.FragmentIndex != 0 || p.FragmentCount != 0 || len(p.Payload) != 0 {
			return errors.New("reliable acknowledgment-only packet is invalid")
		}
		return nil
	}
	if p.FragmentSequence == 0 || p.MessageSequence == 0 {
		return errors.New("reliable data sequence is zero")
	}
	if p.FragmentCount == 0 || p.FragmentCount > MaxReliableFragments || p.FragmentIndex >= p.FragmentCount {
		return errors.New("reliable fragment fields are invalid")
	}
	if len(p.Payload) == 0 || len(p.Payload) > MaxReliablePayload {
		return errors.New("reliable payload length is invalid")
	}
	return nil
}

func validReliableAck(base, bitmap uint64) bool {
	if base == 0 {
		return bitmap == 0
	}
	if bitmap&1 == 0 {
		return false
	}
	return base >= 64 || bitmap>>base == 0
}

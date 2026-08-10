package protocol

import (
	"bytes"
	"crypto"
	"crypto/cipher"
	"crypto/ed25519"
	"encoding/binary"
	"errors"

	"golang.org/x/crypto/chacha20poly1305"
)

type TrafficClass byte

const (
	TrafficAudio TrafficClass = iota + 1
	TrafficInteractive
)

type GroupDatagramHeader struct {
	Class    TrafficClass
	SenderID [32]byte
	StreamID [16]byte
	Sequence uint64
}

type GroupDatagram struct {
	Timestamp uint32
	Payload   []byte
}

func validTrafficClass(class TrafficClass) bool {
	return class >= TrafficAudio && class <= TrafficInteractive
}

func NewGroupDatagramCipher(groupKey [32]byte) cipher.AEAD {
	protector, _ := chacha20poly1305.NewX(groupKey[:])
	return protector
}

func ParseGroupDatagramHeader(packet []byte, expectedRoomTag [16]byte) (GroupDatagramHeader, error) {
	if len(packet) < groupDatagramHeaderSize {
		return GroupDatagramHeader{}, errors.New("group datagram is truncated")
	}
	packetType, roomTag, err := ParsePrefix(packet)
	if err != nil || packetType != PacketGroupDatagram || roomTag != expectedRoomTag {
		return GroupDatagramHeader{}, errors.New("group datagram prefix is invalid")
	}
	header := GroupDatagramHeader{Class: TrafficClass(packet[prefixSize])}
	copy(header.SenderID[:], packet[prefixSize+1:prefixSize+1+32])
	copy(header.StreamID[:], packet[prefixSize+1+32:prefixSize+1+32+16])
	header.Sequence = binary.BigEndian.Uint64(packet[groupDatagramHeaderSize-8 : groupDatagramHeaderSize])
	if !validTrafficClass(header.Class) || header.SenderID == ([32]byte{}) || header.StreamID == ([16]byte{}) || header.Sequence == 0 {
		return GroupDatagramHeader{}, errors.New("group datagram header is invalid")
	}
	return header, nil
}

func groupDatagramNonce(packet []byte) []byte {
	return packet[groupDatagramHeaderSize-chacha20poly1305.NonceSizeX : groupDatagramHeaderSize]
}

func MarshalGroupDatagram(roomTag [16]byte, header GroupDatagramHeader, timestamp uint32, payload []byte, protector cipher.AEAD, signer crypto.Signer) ([]byte, error) {
	if !validTrafficClass(header.Class) || header.SenderID == ([32]byte{}) || header.StreamID == ([16]byte{}) || header.Sequence == 0 {
		return nil, errors.New("group datagram header is invalid")
	}
	if len(payload) == 0 || len(payload) > MaxGroupDatagramPayload {
		return nil, errors.New("group datagram payload length is invalid")
	}
	if protector == nil || protector.NonceSize() != chacha20poly1305.NonceSizeX || protector.Overhead() != aeadTagSize {
		return nil, errors.New("group datagram protector is invalid")
	}
	if signer == nil {
		return nil, errors.New("group datagram signer is invalid")
	}
	publicKey, ok := signer.Public().(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize || !bytes.Equal(publicKey, header.SenderID[:]) {
		return nil, errors.New("group datagram signer does not match sender identity")
	}
	packet := make([]byte, 0, groupDatagramHeaderSize+4+len(payload)+aeadTagSize+groupDatagramSignatureSize)
	packet = appendPrefix(packet, PacketGroupDatagram, roomTag)
	packet = append(packet, byte(header.Class))
	packet = append(packet, header.SenderID[:]...)
	packet = append(packet, header.StreamID[:]...)
	packet = appendUint64(packet, header.Sequence)
	packet = binary.BigEndian.AppendUint32(packet, timestamp)
	packet = append(packet, payload...)
	body := packet[groupDatagramHeaderSize:]
	sealed := protector.Seal(body[:0], groupDatagramNonce(packet), body, packet[:groupDatagramHeaderSize])
	packet = packet[:groupDatagramHeaderSize+len(sealed)]
	signature, err := signer.Sign(nil, packet, crypto.Hash(0))
	if err != nil {
		return nil, errors.New("group datagram signing failed")
	}
	if len(signature) != groupDatagramSignatureSize {
		return nil, errors.New("group datagram signature length is invalid")
	}
	return append(packet, signature...), nil
}

func ParseGroupDatagram(packet []byte, expectedRoomTag [16]byte, expected GroupDatagramHeader, protector cipher.AEAD) (GroupDatagram, error) {
	if len(packet) < groupDatagramMinPacketSize || len(packet) > MaxDatagramSize {
		return GroupDatagram{}, errors.New("group datagram length is invalid")
	}
	if protector == nil || protector.NonceSize() != chacha20poly1305.NonceSizeX || protector.Overhead() != aeadTagSize {
		return GroupDatagram{}, errors.New("group datagram protector is invalid")
	}
	header, err := ParseGroupDatagramHeader(packet, expectedRoomTag)
	if err != nil || header != expected {
		return GroupDatagram{}, errors.New("group datagram header does not match")
	}
	signatureOffset := len(packet) - groupDatagramSignatureSize
	if !ed25519.Verify(ed25519.PublicKey(header.SenderID[:]), packet[:signatureOffset], packet[signatureOffset:]) {
		return GroupDatagram{}, errors.New("group datagram signature verification failed")
	}
	body := append([]byte(nil), packet[groupDatagramHeaderSize:signatureOffset]...)
	opened, err := protector.Open(body[:0], groupDatagramNonce(packet), body, packet[:groupDatagramHeaderSize])
	if err != nil || len(opened) <= 4 {
		return GroupDatagram{}, errors.New("group datagram authentication failed")
	}
	return GroupDatagram{
		Timestamp: binary.BigEndian.Uint32(opened[:4]),
		Payload:   opened[4:],
	}, nil
}

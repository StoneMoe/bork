package iwan

import (
	"crypto/md5"
	"crypto/subtle"
	"errors"
	"time"
)

const (
	DefaultPort = 4567
	DefaultMTU  = 1400
	MinMTU      = 46
	MaxMTU      = 1600

	RetryInterval      = 2 * time.Second
	AuthTimeout        = 6 * time.Second
	EstablishedTimeout = 15 * time.Second
	FragmentTimeout    = 5 * time.Second
	FragmentSlots      = 10

	headerSize         = 8
	signatureSize      = md5.Size
	signedHeaderSize   = headerSize + signatureSize
	fragmentHeaderSize = signedHeaderSize
	fragmentOutputSize = 4096
)

type PacketType byte

const (
	TypeOpenReject   PacketType = 0x11
	TypeOpenACK      PacketType = 0x12
	TypeOpen         PacketType = 0x13
	TypeData         PacketType = 0x14
	TypeEchoRequest  PacketType = 0x15
	TypeEchoResponse PacketType = 0x16
	TypeClose        PacketType = 0x17
	TypeDataXOR      PacketType = 0x18
	TypeIPFragment   PacketType = 0x21
)

type Token [2]byte
type SessionID [4]byte

var (
	ErrMalformedPacket   = errors.New("iwan: malformed packet")
	ErrInvalidSignature  = errors.New("iwan: invalid control signature")
	ErrUnknownPacketType = errors.New("iwan: unknown packet type")
	ErrSessionMismatch   = errors.New("iwan: session mismatch")
	ErrOversizedPacket   = errors.New("iwan: oversized packet")
	ErrFragmentOverlap   = errors.New("iwan: fragment overlap")
	ErrFragmentConflict  = errors.New("iwan: fragment conflict")
	ErrFragmentOverflow  = errors.New("iwan: fragment overflow")
	ErrFragmentSlotsFull = errors.New("iwan: fragment slots full")
)

type wireHeader struct {
	typ     PacketType
	flags   byte
	token   Token
	session SessionID
}

func parseHeader(packet []byte) (wireHeader, error) {
	if len(packet) < headerSize {
		return wireHeader{}, ErrMalformedPacket
	}
	return wireHeader{
		typ:     PacketType(packet[0]),
		flags:   packet[1],
		token:   Token(packet[2:4]),
		session: SessionID(packet[4:8]),
	}, nil
}

func writeHeader(packet []byte, header wireHeader) {
	packet[0] = byte(header.typ)
	packet[1] = header.flags
	copy(packet[2:4], header.token[:])
	copy(packet[4:8], header.session[:])
}

// The MD5 field is compatibility obfuscation over the header only; it provides
// no payload integrity, peer authentication, or replay protection.
func signControl(packet []byte) {
	digestInput := make([]byte, 0, headerSize+2)
	digestInput = append(digestInput, packet[:headerSize]...)
	digestInput = append(digestInput, 'm', 'w')
	digest := md5.Sum(digestInput)
	copy(packet[headerSize:signedHeaderSize], digest[:])
}

func validateSignedControl(packet []byte) error {
	if len(packet) < signedHeaderSize {
		return ErrMalformedPacket
	}
	digestInput := make([]byte, 0, headerSize+2)
	digestInput = append(digestInput, packet[:headerSize]...)
	digestInput = append(digestInput, 'm', 'w')
	digest := md5.Sum(digestInput)
	if subtle.ConstantTimeCompare(packet[headerSize:signedHeaderSize], digest[:]) != 1 {
		return ErrInvalidSignature
	}
	return nil
}

type tlv struct {
	typ   byte
	value []byte
}

func parseTLVs(data []byte) ([]tlv, error) {
	items := make([]tlv, 0, 6)
	for len(data) > 0 {
		if len(data) < 2 {
			return nil, ErrMalformedPacket
		}
		length := int(data[1])
		if length < 2 || length > len(data) {
			return nil, ErrMalformedPacket
		}
		items = append(items, tlv{typ: data[0], value: data[2:length]})
		data = data[length:]
	}
	return items, nil
}

func appendTLV(packet []byte, typ byte, value ...byte) []byte {
	packet = append(packet, typ, byte(len(value)+2))
	return append(packet, value...)
}

func isCriticalTLV(typ byte) bool {
	return typ&0x80 != 0
}

func allZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}

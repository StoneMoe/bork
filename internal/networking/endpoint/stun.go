package endpoint

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

const (
	stunHeaderSize              = 20
	stunMagicCookie      uint32 = 0x2112a442
	stunBindingRequest          = 0x0001
	stunBindingSuccess          = 0x0101
	stunBindingError            = 0x0111
	stunMappedAddress           = 0x0001
	stunXORMappedAddress        = 0x0020
)

type stunTransaction [12]byte

func newBindingRequest() (stunTransaction, []byte, error) {
	var transaction stunTransaction
	if _, err := rand.Read(transaction[:]); err != nil {
		return stunTransaction{}, nil, fmt.Errorf("generate STUN transaction: %w", err)
	}
	message := make([]byte, stunHeaderSize)
	binary.BigEndian.PutUint16(message[0:2], stunBindingRequest)
	binary.BigEndian.PutUint32(message[4:8], stunMagicCookie)
	copy(message[8:20], transaction[:])
	return transaction, message, nil
}

func parseBindingResponse(message []byte, transaction stunTransaction) (netip.AddrPort, error) {
	if len(message) < stunHeaderSize {
		return netip.AddrPort{}, errors.New("STUN response is truncated")
	}
	messageType := binary.BigEndian.Uint16(message[0:2])
	if messageType == stunBindingError {
		return netip.AddrPort{}, errors.New("STUN server returned an error response")
	}
	if messageType != stunBindingSuccess {
		return netip.AddrPort{}, fmt.Errorf("unexpected STUN response type 0x%04x", messageType)
	}
	if binary.BigEndian.Uint32(message[4:8]) != stunMagicCookie {
		return netip.AddrPort{}, errors.New("STUN response has an invalid magic cookie")
	}
	if string(message[8:20]) != string(transaction[:]) {
		return netip.AddrPort{}, errors.New("STUN response transaction does not match")
	}
	declaredLength := int(binary.BigEndian.Uint16(message[2:4]))
	if declaredLength%4 != 0 || declaredLength != len(message)-stunHeaderSize {
		return netip.AddrPort{}, errors.New("STUN response length is invalid")
	}

	var mapped netip.AddrPort
	for offset := stunHeaderSize; offset < len(message); {
		if len(message)-offset < 4 {
			return netip.AddrPort{}, errors.New("STUN attribute header is truncated")
		}
		attributeType := binary.BigEndian.Uint16(message[offset : offset+2])
		attributeLength := int(binary.BigEndian.Uint16(message[offset+2 : offset+4]))
		valueStart := offset + 4
		valueEnd := valueStart + attributeLength
		if valueEnd > len(message) {
			return netip.AddrPort{}, errors.New("STUN attribute value is truncated")
		}
		if attributeType == stunXORMappedAddress {
			return parseMappedAddress(message[valueStart:valueEnd], transaction, true)
		}
		if attributeType == stunMappedAddress {
			address, err := parseMappedAddress(message[valueStart:valueEnd], transaction, false)
			if err == nil {
				mapped = address
			}
		}
		offset = valueStart + ((attributeLength + 3) &^ 3)
		if offset > len(message) {
			return netip.AddrPort{}, errors.New("STUN attribute padding is truncated")
		}
	}
	if mapped.IsValid() {
		return mapped, nil
	}
	return netip.AddrPort{}, errors.New("STUN response does not contain a mapped address")
}

func parseMappedAddress(value []byte, transaction stunTransaction, xor bool) (netip.AddrPort, error) {
	if len(value) < 4 || value[0] != 0 {
		return netip.AddrPort{}, errors.New("STUN mapped address is malformed")
	}
	port := binary.BigEndian.Uint16(value[2:4])
	if xor {
		port ^= uint16(stunMagicCookie >> 16)
	}

	var address netip.Addr
	switch value[1] {
	case 0x01:
		if len(value) != 8 {
			return netip.AddrPort{}, errors.New("STUN IPv4 mapped address length is invalid")
		}
		var bytes [4]byte
		copy(bytes[:], value[4:8])
		if xor {
			cookie := stunMagicCookie
			for index := range bytes {
				bytes[index] ^= byte(cookie >> (24 - 8*index))
			}
		}
		address = netip.AddrFrom4(bytes)
	case 0x02:
		if len(value) != 20 {
			return netip.AddrPort{}, errors.New("STUN IPv6 mapped address length is invalid")
		}
		var bytes [16]byte
		copy(bytes[:], value[4:20])
		if xor {
			mask := make([]byte, 16)
			binary.BigEndian.PutUint32(mask[:4], stunMagicCookie)
			copy(mask[4:], transaction[:])
			for index := range bytes {
				bytes[index] ^= mask[index]
			}
		}
		address = netip.AddrFrom16(bytes)
	default:
		return netip.AddrPort{}, errors.New("STUN mapped address family is unsupported")
	}
	if !address.IsValid() || address.IsUnspecified() || port == 0 {
		return netip.AddrPort{}, errors.New("STUN mapped address is unusable")
	}
	return netip.AddrPortFrom(address.Unmap(), port), nil
}

func stunTransactionFromMessage(message []byte) (stunTransaction, bool) {
	var transaction stunTransaction
	if len(message) < stunHeaderSize || binary.BigEndian.Uint32(message[4:8]) != stunMagicCookie {
		return transaction, false
	}
	copy(transaction[:], message[8:20])
	return transaction, true
}

package endpoint

import (
	"encoding/binary"
	"net/netip"
	"testing"
)

func TestParseBindingResponseIPv4(t *testing.T) {
	transaction := stunTransaction{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	mapped := netip.MustParseAddrPort("203.0.113.7:54321")
	message := bindingResponse(transaction, mapped)
	got, err := parseBindingResponse(message, transaction)
	if err != nil {
		t.Fatalf("parseBindingResponse() error = %v", err)
	}
	if got != mapped {
		t.Fatalf("parseBindingResponse() = %s, want %s", got, mapped)
	}
}

func TestParseBindingResponseIPv6(t *testing.T) {
	transaction := stunTransaction{12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}
	mapped := netip.MustParseAddrPort("[2001:db8::7]:3478")
	message := bindingResponse(transaction, mapped)
	got, err := parseBindingResponse(message, transaction)
	if err != nil {
		t.Fatalf("parseBindingResponse() error = %v", err)
	}
	if got != mapped {
		t.Fatalf("parseBindingResponse() = %s, want %s", got, mapped)
	}
}

func TestParseBindingResponseRejectsTruncatedAttribute(t *testing.T) {
	transaction := stunTransaction{1}
	message := bindingResponse(transaction, netip.MustParseAddrPort("203.0.113.7:5000"))
	message = message[:len(message)-1]
	if _, err := parseBindingResponse(message, transaction); err == nil {
		t.Fatal("parseBindingResponse() error = nil")
	}
}

func bindingResponse(transaction stunTransaction, mapped netip.AddrPort) []byte {
	address := mapped.Addr().Unmap()
	attributeLength := 8
	family := byte(0x01)
	if address.Is6() {
		attributeLength = 20
		family = 0x02
	}
	message := make([]byte, stunHeaderSize+4+attributeLength)
	binary.BigEndian.PutUint16(message[0:2], stunBindingSuccess)
	binary.BigEndian.PutUint16(message[2:4], uint16(4+attributeLength))
	binary.BigEndian.PutUint32(message[4:8], stunMagicCookie)
	copy(message[8:20], transaction[:])
	binary.BigEndian.PutUint16(message[20:22], stunXORMappedAddress)
	binary.BigEndian.PutUint16(message[22:24], uint16(attributeLength))
	message[25] = family
	binary.BigEndian.PutUint16(message[26:28], mapped.Port()^uint16(stunMagicCookie>>16))
	bytes := address.AsSlice()
	mask := make([]byte, len(bytes))
	binary.BigEndian.PutUint32(mask[:4], stunMagicCookie)
	if len(mask) == 16 {
		copy(mask[4:], transaction[:])
	}
	for index := range bytes {
		message[28+index] = bytes[index] ^ mask[index]
	}
	return message
}

func FuzzParseBindingResponse(f *testing.F) {
	transaction := stunTransaction{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	f.Add(bindingResponse(transaction, netip.MustParseAddrPort("203.0.113.7:54321")))
	f.Add([]byte("not-stun"))
	f.Fuzz(func(t *testing.T, message []byte) {
		_, _ = parseBindingResponse(message, transaction)
	})
}

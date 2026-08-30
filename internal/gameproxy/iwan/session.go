package iwan

import (
	"fmt"
	"net/netip"
)

type Session struct {
	Token   Token
	ID      SessionID
	MTU     uint16
	Address netip.Addr
	DNS     []netip.Addr
	xorKey  [8]byte
}

func ParseOpenACK(packet []byte, credentials Credentials) (Session, error) {
	if !credentials.valid {
		return Session{}, ErrMalformedPacket
	}
	header, err := parseHeader(packet)
	if err != nil {
		return Session{}, err
	}
	if header.typ != TypeOpenACK || header.flags != 1 || allZero(header.token[:]) || allZero(header.session[:]) {
		return Session{}, ErrMalformedPacket
	}
	if err := validateSignedControl(packet); err != nil {
		return Session{}, err
	}
	items, err := parseTLVs(packet[signedHeaderSize:])
	if err != nil {
		return Session{}, err
	}
	session := Session{Token: header.token, ID: header.session, xorKey: credentials.xorKey}
	var mtuSeen, addressSeen, xorSeen, dnsSingleSeen, dnsPairSeen bool
	var dnsSingle []netip.Addr
	for _, item := range items {
		switch item.typ {
		case 3:
			if mtuSeen || len(item.value) != 2 {
				return Session{}, ErrMalformedPacket
			}
			session.MTU = uint16(item.value[0])<<8 | uint16(item.value[1])
			mtuSeen = true
		case 4:
			if addressSeen || len(item.value) != 4 {
				return Session{}, ErrMalformedPacket
			}
			session.Address = netip.AddrFrom4([4]byte(item.value))
			addressSeen = true
		case 5:
			if dnsSingleSeen || len(item.value) != 4 {
				return Session{}, ErrMalformedPacket
			}
			dnsSingle = []netip.Addr{netip.AddrFrom4([4]byte(item.value))}
			dnsSingleSeen = true
		case 6:
			if dnsPairSeen || len(item.value) != 8 {
				return Session{}, ErrMalformedPacket
			}
			session.DNS = []netip.Addr{
				netip.AddrFrom4([4]byte(item.value[:4])),
				netip.AddrFrom4([4]byte(item.value[4:8])),
			}
			dnsPairSeen = true
		case 8:
			if xorSeen {
				return Session{}, ErrMalformedPacket
			}
			if len(item.value) != 1 || item.value[0] != 1 {
				return Session{}, ErrProtocolDowngrade
			}
			xorSeen = true
		default:
			if isCriticalTLV(item.typ) {
				return Session{}, ErrMalformedPacket
			}
		}
	}
	if !dnsPairSeen {
		session.DNS = dnsSingle
	}
	if dnsSingleSeen && !validIPv4Endpoint(dnsSingle[0]) {
		return Session{}, ErrMalformedPacket
	}
	if !xorSeen {
		return Session{}, ErrProtocolDowngrade
	}
	if !mtuSeen || session.MTU < MinMTU || session.MTU > MaxMTU || !addressSeen || !validIPv4Endpoint(session.Address) {
		return Session{}, ErrMalformedPacket
	}
	for _, dns := range session.DNS {
		if !validIPv4Endpoint(dns) {
			return Session{}, ErrMalformedPacket
		}
	}
	return session, nil
}

func (session Session) BuildData(payload []byte) ([]byte, error) {
	if len(payload) == 0 {
		return nil, ErrMalformedPacket
	}
	if len(payload) > int(session.MTU) {
		return nil, ErrOversizedPacket
	}
	packet := make([]byte, headerSize+len(payload))
	writeHeader(packet, wireHeader{typ: TypeDataXOR, flags: 1, token: session.Token, session: session.ID})
	copy(packet[headerSize:], payload)
	xorBytes(session.xorKey, packet[headerSize:])
	return packet, nil
}

func (session Session) ParseData(packet []byte) ([]byte, error) {
	header, err := parseHeader(packet)
	if err != nil {
		return nil, err
	}
	if header.typ == TypeData {
		return nil, fmt.Errorf("plaintext DATA: %w", ErrMalformedPacket)
	}
	if header.typ != TypeDataXOR || header.flags != 1 {
		return nil, ErrUnknownPacketType
	}
	if header.token != session.Token || header.session != session.ID {
		return nil, ErrSessionMismatch
	}
	payloadSize := len(packet) - headerSize
	if payloadSize <= 0 {
		return nil, ErrMalformedPacket
	}
	if payloadSize > int(session.MTU) {
		return nil, ErrOversizedPacket
	}
	payload := append([]byte(nil), packet[headerSize:]...)
	xorBytes(session.xorKey, payload)
	return payload, nil
}

// XOR is the protocol's reversible obfuscation only; DATA has no integrity or replay protection.
func xorBytes(key [8]byte, data []byte) {
	for index := range data {
		data[index] ^= key[index&7]
	}
}

func validIPv4Endpoint(address netip.Addr) bool {
	return address.Is4() && !address.IsUnspecified() && !address.IsMulticast()
}

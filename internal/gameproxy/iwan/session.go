//go:build game_proxy

package iwan

import "net/netip"

type Session struct {
	Token   Token
	ID      SessionID
	MTU     uint16
	Address netip.Addr
	DNS     []netip.Addr
	Encrypt bool
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
	if header.typ != TypeOpenACK {
		return Session{}, ErrMalformedPacket
	}
	if err := validateSignedControl(packet); err != nil {
		return Session{}, err
	}
	session := Session{
		Token: header.token, ID: header.session, Encrypt: header.flags != 0,
		xorKey: credentials.xorKey,
	}
	for tlvs := packet[signedHeaderSize:]; len(tlvs) > 0; {
		if len(tlvs) < 2 {
			break
		}
		length := int(tlvs[1])
		if length < 2 || length > len(tlvs) {
			break
		}
		value := tlvs[2:length]
		switch tlvs[0] {
		case 3:
			if len(value) >= 2 {
				session.MTU = uint16(value[0])<<8 | uint16(value[1])
			}
		case 4:
			if len(value) >= 4 {
				session.Address = netip.AddrFrom4([4]byte(value[:4]))
			}
		case 5:
			if len(value) >= 4 {
				session.DNS = []netip.Addr{netip.AddrFrom4([4]byte(value[:4]))}
			}
		case 6:
			if len(value) >= 4 {
				session.DNS = []netip.Addr{netip.AddrFrom4([4]byte(value[:4]))}
			}
			if len(value) >= 8 {
				session.DNS = append(session.DNS, netip.AddrFrom4([4]byte(value[4:8])))
			}
		case 8:
			if len(value) >= 1 {
				session.Encrypt = value[0] != 0
			}
		}
		tlvs = tlvs[length:]
	}
	if !session.Address.IsValid() || session.Address.IsUnspecified() {
		return Session{}, ErrMalformedPacket
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
	header := wireHeader{typ: TypeData, token: session.Token, session: session.ID}
	if session.Encrypt {
		header.typ = TypeDataXOR
		header.flags = 1
	}
	writeHeader(packet, header)
	copy(packet[headerSize:], payload)
	if session.Encrypt {
		xorBytes(session.xorKey, packet[headerSize:])
	}
	return packet, nil
}

func (session Session) ParseData(packet []byte) ([]byte, error) {
	header, err := parseHeader(packet)
	if err != nil {
		return nil, err
	}
	if header.typ != TypeData && header.typ != TypeDataXOR {
		return nil, ErrUnknownPacketType
	}
	if len(packet) == headerSize {
		return nil, ErrMalformedPacket
	}
	payload := append([]byte(nil), packet[headerSize:]...)
	if header.typ == TypeDataXOR {
		xorBytes(session.xorKey, payload)
	}
	return payload, nil
}

// XOR is the protocol's reversible obfuscation only; DATA has no integrity or replay protection.
func xorBytes(key [8]byte, data []byte) {
	for index := range data {
		data[index] ^= key[index&7]
	}
}

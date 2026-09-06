//go:build game_proxy

package iwan

import (
	"encoding/binary"
	"math"
	"time"
)

const echoPayloadSize = 36

type Echo struct {
	Timestamp    time.Time
	CurrentDelay uint32
	MinimumDelay uint32
	MaximumDelay uint32
	RouteMagic   uint32
}

type ControlPacket struct {
	Type PacketType
	Echo Echo
}

func BuildEchoRequest(session Session, echo Echo) []byte {
	packet := make([]byte, signedHeaderSize+echoPayloadSize)
	writeHeader(packet, wireHeader{typ: TypeEchoRequest, token: session.Token, session: session.ID})
	signControl(packet)
	writeEcho(packet[signedHeaderSize:], echo)
	return packet
}

func BuildEchoResponse(session Session, request []byte) ([]byte, error) {
	control, err := ParseControl(request, session)
	if err != nil {
		return nil, err
	}
	if control.Type != TypeEchoRequest {
		return nil, ErrUnknownPacketType
	}
	packet := append([]byte(nil), request...)
	packet[0] = byte(TypeEchoResponse)
	signControl(packet)
	return packet, nil
}

func BuildClose(session Session) []byte {
	packet := make([]byte, signedHeaderSize)
	writeHeader(packet, wireHeader{typ: TypeClose, token: session.Token, session: session.ID})
	signControl(packet)
	return packet
}

func BuildOpenReject() []byte {
	packet := make([]byte, signedHeaderSize)
	writeHeader(packet, wireHeader{typ: TypeOpenReject})
	signControl(packet)
	return packet
}

func ParseOpenReject(packet []byte) error {
	if len(packet) != signedHeaderSize {
		return ErrMalformedPacket
	}
	header, err := parseHeader(packet)
	if err != nil {
		return err
	}
	if header.typ != TypeOpenReject || header.flags != 0 || !allZero(header.token[:]) || !allZero(header.session[:]) {
		return ErrMalformedPacket
	}
	return validateSignedControl(packet)
}

func ParseControl(packet []byte, session Session) (ControlPacket, error) {
	if len(packet) < signedHeaderSize {
		return ControlPacket{}, ErrMalformedPacket
	}
	header, err := parseHeader(packet)
	if err != nil {
		return ControlPacket{}, err
	}
	if err := validateSignedControl(packet); err != nil {
		return ControlPacket{}, err
	}
	if header.typ != TypeEchoResponse {
		if header.token != session.Token || header.session != session.ID {
			return ControlPacket{}, ErrSessionMismatch
		}
		if header.flags != 0 {
			return ControlPacket{}, ErrMalformedPacket
		}
	}
	switch header.typ {
	case TypeEchoRequest, TypeEchoResponse:
		var echo Echo
		if len(packet) >= signedHeaderSize+8 {
			timestamp := binary.LittleEndian.Uint64(packet[signedHeaderSize : signedHeaderSize+8])
			if timestamp <= math.MaxInt64 {
				echo.Timestamp = time.UnixMicro(int64(timestamp))
			}
		}
		if len(packet) >= signedHeaderSize+20 {
			payload := packet[signedHeaderSize:]
			echo.CurrentDelay = binary.LittleEndian.Uint32(payload[8:12])
			echo.MinimumDelay = binary.LittleEndian.Uint32(payload[12:16])
			echo.MaximumDelay = binary.LittleEndian.Uint32(payload[16:20])
		}
		if len(packet) >= signedHeaderSize+32 {
			echo.RouteMagic = binary.BigEndian.Uint32(packet[signedHeaderSize+28 : signedHeaderSize+32])
		}
		return ControlPacket{Type: header.typ, Echo: echo}, nil
	case TypeClose:
		if len(packet) != signedHeaderSize {
			return ControlPacket{}, ErrMalformedPacket
		}
		return ControlPacket{Type: TypeClose}, nil
	default:
		return ControlPacket{}, ErrUnknownPacketType
	}
}

func writeEcho(payload []byte, echo Echo) {
	binary.LittleEndian.PutUint64(payload[:8], uint64(echo.Timestamp.UnixMicro()))
	binary.LittleEndian.PutUint32(payload[8:12], echo.CurrentDelay)
	binary.LittleEndian.PutUint32(payload[12:16], echo.MinimumDelay)
	binary.LittleEndian.PutUint32(payload[16:20], echo.MaximumDelay)
	copy(payload[24:28], []byte{'T', 'D', 'R', 0})
	binary.BigEndian.PutUint32(payload[28:32], echo.RouteMagic)
}

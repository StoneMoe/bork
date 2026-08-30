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
	if header.token != session.Token || header.session != session.ID {
		return ControlPacket{}, ErrSessionMismatch
	}
	if header.flags != 0 {
		return ControlPacket{}, ErrMalformedPacket
	}
	switch header.typ {
	case TypeEchoRequest, TypeEchoResponse:
		if len(packet) != signedHeaderSize+echoPayloadSize {
			return ControlPacket{}, ErrMalformedPacket
		}
		echo, err := parseEcho(packet[signedHeaderSize:])
		if err != nil {
			return ControlPacket{}, err
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

func parseEcho(payload []byte) (Echo, error) {
	timestamp := binary.LittleEndian.Uint64(payload[:8])
	if timestamp > math.MaxInt64 || payload[20] != 0 || payload[21] != 0 || payload[22] != 0 || payload[23] != 0 || string(payload[24:28]) != "TDR\x00" || payload[32] != 0 || payload[33] != 0 || payload[34] != 0 || payload[35] != 0 {
		return Echo{}, ErrMalformedPacket
	}
	return Echo{
		Timestamp:    time.UnixMicro(int64(timestamp)),
		CurrentDelay: binary.LittleEndian.Uint32(payload[8:12]),
		MinimumDelay: binary.LittleEndian.Uint32(payload[12:16]),
		MaximumDelay: binary.LittleEndian.Uint32(payload[16:20]),
		RouteMagic:   binary.BigEndian.Uint32(payload[28:32]),
	}, nil
}

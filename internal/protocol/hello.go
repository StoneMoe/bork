package protocol

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"

	"bork/internal/identity"
)

// SessionHello is one side of a Session transcript. The initiator chooses the
// SessionID, and both peers keep the encoded packet for the Session lifetime.
type SessionHello struct {
	SessionID    [16]byte
	PeerID       identity.PeerID
	EphemeralKey [32]byte
	wire         [sessionHelloPacketSize]byte
}

// MarshalHelloProbe announces a room member on a candidate path. It is not
// part of a Session transcript and therefore carries no Session key material.
func MarshalHelloProbe(admissionKey [32]byte, peerID identity.PeerID) ([]byte, error) {
	if peerID.IsZero() {
		return nil, errors.New("hello probe peer ID is empty")
	}
	packet := make([]byte, 0, helloProbePacketSize)
	packet = appendPrefix(packet, PacketHelloProbe)
	packet = append(packet, peerID[:]...)
	return appendAdmissionMAC(packet, admissionKey), nil
}

func ParseHelloProbe(packet []byte, admissionKey [32]byte) (identity.PeerID, error) {
	if len(packet) != helloProbePacketSize {
		return identity.PeerID{}, errors.New("hello probe length is invalid")
	}
	packetType, err := ParsePrefix(packet)
	if err != nil || packetType != PacketHelloProbe || !validAdmissionMAC(packet, admissionKey) {
		return identity.PeerID{}, errors.New("hello probe authentication failed")
	}
	var peerID identity.PeerID
	copy(peerID[:], packet[prefixSize:prefixSize+len(peerID)])
	if peerID.IsZero() {
		return identity.PeerID{}, errors.New("hello probe peer ID is empty")
	}
	return peerID, nil
}

func MarshalSessionHello(admissionKey [32]byte, peerID identity.PeerID, sessionID [16]byte, ephemeralKey [32]byte) ([]byte, error) {
	if peerID.IsZero() {
		return nil, errors.New("session hello peer ID is empty")
	}
	if sessionID == ([16]byte{}) {
		return nil, errors.New("session hello session ID is empty")
	}
	if ephemeralKey == ([32]byte{}) {
		return nil, errors.New("session hello ephemeral key is empty")
	}
	packet := make([]byte, 0, sessionHelloPacketSize)
	packet = appendPrefix(packet, PacketSessionHello)
	packet = append(packet, sessionID[:]...)
	packet = append(packet, peerID[:]...)
	packet = append(packet, ephemeralKey[:]...)
	return appendAdmissionMAC(packet, admissionKey), nil
}

func ParseSessionHello(packet []byte, admissionKey [32]byte) (SessionHello, error) {
	if len(packet) != sessionHelloPacketSize {
		return SessionHello{}, errors.New("session hello length is invalid")
	}
	packetType, err := ParsePrefix(packet)
	if err != nil || packetType != PacketSessionHello || !validAdmissionMAC(packet, admissionKey) {
		return SessionHello{}, errors.New("session hello authentication failed")
	}

	var hello SessionHello
	offset := prefixSize
	copy(hello.SessionID[:], packet[offset:offset+len(hello.SessionID)])
	offset += len(hello.SessionID)
	copy(hello.PeerID[:], packet[offset:offset+len(hello.PeerID)])
	offset += len(hello.PeerID)
	copy(hello.EphemeralKey[:], packet[offset:offset+len(hello.EphemeralKey)])
	if hello.SessionID == ([16]byte{}) || hello.PeerID.IsZero() || hello.EphemeralKey == ([32]byte{}) {
		return SessionHello{}, errors.New("session hello fields are invalid")
	}
	copy(hello.wire[:], packet)
	return hello, nil
}

func appendAdmissionMAC(packet []byte, admissionKey [32]byte) []byte {
	mac := hmac.New(sha256.New, admissionKey[:])
	_, _ = mac.Write(packet)
	return mac.Sum(packet)
}

func validAdmissionMAC(packet []byte, admissionKey [32]byte) bool {
	bodyEnd := len(packet) - helloMACSize
	mac := hmac.New(sha256.New, admissionKey[:])
	_, _ = mac.Write(packet[:bodyEnd])
	return hmac.Equal(packet[bodyEnd:], mac.Sum(nil))
}

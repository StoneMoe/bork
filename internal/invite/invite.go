package invite

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/hkdf"
)

const (
	Version          = 1
	RoomSeedSize     = 32
	TrackerHashSize  = 20
	MaxDisplayRunes  = 64
	MaxDisplayBytes  = 256
	MaxEncodedSize   = 512
	prefix           = "bork://join/"
	payloadFixedSize = 1 + RoomSeedSize
	checksumSize     = 2
	hkdfSalt         = "bork/invite/hkdf-sha256/v1"
)

type Invite struct {
	DisplayName string
	roomSeed    [RoomSeedSize]byte
}

func New(displayName string) (Invite, error) {
	displayName, err := validateDisplayName(displayName)
	if err != nil {
		return Invite{}, err
	}
	invite := Invite{DisplayName: displayName}
	if _, err := rand.Read(invite.roomSeed[:]); err != nil {
		return Invite{}, fmt.Errorf("generate room seed: %w", err)
	}
	return invite, nil
}

func Parse(encoded string) (Invite, error) {
	if len(encoded) > MaxEncodedSize {
		return Invite{}, errors.New("invite is too large")
	}
	encoded = strings.TrimSpace(encoded)
	encoded = strings.TrimPrefix(encoded, prefix)
	if encoded == "" {
		return Invite{}, errors.New("invite is empty")
	}
	if strings.ContainsAny(encoded, "\r\n") {
		return Invite{}, errors.New("invite encoding is invalid")
	}
	payload, err := decodeBase58(encoded)
	if err != nil {
		return Invite{}, errors.New("invite encoding is invalid")
	}
	if encodeBase58(payload) != encoded {
		return Invite{}, errors.New("invite encoding is not canonical")
	}
	if len(payload) < payloadFixedSize+checksumSize {
		return Invite{}, errors.New("invite payload is truncated")
	}
	content := payload[:len(payload)-checksumSize]
	if payload[0] != Version {
		return Invite{}, fmt.Errorf("unsupported invite version %d", payload[0])
	}
	if binary.BigEndian.Uint16(payload[len(payload)-checksumSize:]) != inviteCRC16(content) {
		return Invite{}, errors.New("invite checksum is invalid")
	}
	rawDisplayName := string(content[payloadFixedSize:])
	displayName, err := validateDisplayName(rawDisplayName)
	if err != nil {
		return Invite{}, err
	}
	if displayName != rawDisplayName {
		return Invite{}, errors.New("invite room name is not canonical")
	}
	invite := Invite{DisplayName: displayName}
	copy(invite.roomSeed[:], payload[1:1+RoomSeedSize])
	return invite, nil
}

func (i Invite) Encode() string {
	payload := make([]byte, payloadFixedSize+len(i.DisplayName)+checksumSize)
	payload[0] = Version
	copy(payload[1:1+RoomSeedSize], i.roomSeed[:])
	copy(payload[payloadFixedSize:], i.DisplayName)
	binary.BigEndian.PutUint16(
		payload[len(payload)-checksumSize:],
		inviteCRC16(payload[:len(payload)-checksumSize]),
	)
	return prefix + encodeBase58(payload)
}

// inviteCRC16 implements CRC-16/CCITT-FALSE for accidental input errors.
func inviteCRC16(content []byte) uint16 {
	crc := uint16(0xffff)
	for _, value := range content {
		crc ^= uint16(value) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// TrackerHash returns the non-enumerable swarm identifier used for tracker discovery.
func (i Invite) TrackerHash() [TrackerHashSize]byte {
	derived := i.derive("bork/tracker/v1", TrackerHashSize)
	var hash [TrackerHashSize]byte
	copy(hash[:], derived)
	return hash
}

// RoomDatagramKey returns the room-scoped symmetric key that seals realtime
// room datagrams. It is a room transport key, not forward-secret group E2EE:
// every RoomSeed holder can decrypt, while signatures bind packets to a
// transient PeerID for forwarding and replay checks, not member authorization.
func (i Invite) RoomDatagramKey() [32]byte {
	derived := i.derive("bork/room-datagram/v1", 32)
	var key [32]byte
	copy(key[:], derived)
	return key
}

// AdmissionKey returns the secret key used to prove possession of the room invite.
func (i Invite) AdmissionKey() [sha256.Size]byte {
	derived := i.derive("bork/admission/v1", sha256.Size)
	var key [sha256.Size]byte
	copy(key[:], derived)
	return key
}

// RoomTag returns the compact public tag used to route packets and filter discovery results.
func (i Invite) RoomTag() [16]byte {
	derived := i.derive("bork/room-tag/v1", 16)
	var tag [16]byte
	copy(tag[:], derived)
	return tag
}

func (i Invite) derive(info string, length int) []byte {
	derived := make([]byte, length)
	if _, err := io.ReadFull(hkdf.New(sha256.New, i.roomSeed[:], []byte(hkdfSalt), []byte(info)), derived); err != nil {
		panic(err)
	}
	return derived
}

func validateDisplayName(displayName string) (string, error) {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return "", errors.New("room name is required")
	}
	if !utf8.ValidString(displayName) || utf8.RuneCountInString(displayName) > MaxDisplayRunes || len(displayName) > MaxDisplayBytes {
		return "", fmt.Errorf("room name must contain at most %d characters", MaxDisplayRunes)
	}
	return displayName, nil
}

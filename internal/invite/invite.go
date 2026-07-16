package invite

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
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
	MaxEncodedSize   = 1024
	prefix           = "bork://join/"
	payloadFixedSize = 1 + RoomSeedSize + 2
	checksumSize     = 4
	hkdfSalt         = "bork/invite/hkdf-sha256/v1"
)

type Invite struct {
	Version     uint8  `json:"version"`
	DisplayName string `json:"displayName"`
	roomSeed    [RoomSeedSize]byte
}

func New(displayName string) (Invite, error) {
	displayName, err := validateDisplayName(displayName)
	if err != nil {
		return Invite{}, err
	}
	invite := Invite{Version: Version, DisplayName: displayName}
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
	payload, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return Invite{}, errors.New("invite encoding is invalid")
	}
	if base64.RawURLEncoding.EncodeToString(payload) != encoded {
		return Invite{}, errors.New("invite encoding is not canonical")
	}
	if len(payload) < payloadFixedSize+checksumSize {
		return Invite{}, errors.New("invite payload is truncated")
	}
	if payload[0] != Version {
		return Invite{}, fmt.Errorf("unsupported invite version %d", payload[0])
	}
	nameLength := int(binary.BigEndian.Uint16(payload[1+RoomSeedSize : payloadFixedSize]))
	if len(payload) != payloadFixedSize+nameLength+checksumSize {
		return Invite{}, errors.New("invite payload length is invalid")
	}
	content := payload[:len(payload)-checksumSize]
	wantChecksum := sha256.Sum256(content)
	if string(payload[len(payload)-checksumSize:]) != string(wantChecksum[:checksumSize]) {
		return Invite{}, errors.New("invite checksum is invalid")
	}
	displayName, err := validateDisplayName(string(content[payloadFixedSize:]))
	if err != nil {
		return Invite{}, err
	}
	invite := Invite{Version: Version, DisplayName: displayName}
	copy(invite.roomSeed[:], payload[1:1+RoomSeedSize])
	return invite, nil
}

func (i Invite) Encode() string {
	payload := make([]byte, payloadFixedSize+len(i.DisplayName)+checksumSize)
	payload[0] = i.Version
	copy(payload[1:1+RoomSeedSize], i.roomSeed[:])
	binary.BigEndian.PutUint16(payload[1+RoomSeedSize:payloadFixedSize], uint16(len(i.DisplayName)))
	copy(payload[payloadFixedSize:], i.DisplayName)
	checksum := sha256.Sum256(payload[:len(payload)-checksumSize])
	copy(payload[len(payload)-checksumSize:], checksum[:checksumSize])
	return prefix + base64.RawURLEncoding.EncodeToString(payload)
}

// TrackerHash returns the non-enumerable swarm identifier used for tracker discovery.
func (i Invite) TrackerHash() [TrackerHashSize]byte {
	derived := i.derive("bork/tracker/v1", TrackerHashSize)
	var hash [TrackerHashSize]byte
	copy(hash[:], derived)
	return hash
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

func (i Invite) Equal(other Invite) bool {
	return i.Version == other.Version && i.DisplayName == other.DisplayName && i.roomSeed == other.roomSeed
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

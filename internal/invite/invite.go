package invite

import (
	"crypto/rand"
	"crypto/sha256"
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
	checksumSize     = 4
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
	if payload[0] != Version {
		return Invite{}, fmt.Errorf("unsupported invite version %d", payload[0])
	}
	content := payload[:len(payload)-checksumSize]
	wantChecksum := sha256.Sum256(content)
	if string(payload[len(payload)-checksumSize:]) != string(wantChecksum[:checksumSize]) {
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
	checksum := sha256.Sum256(payload[:len(payload)-checksumSize])
	copy(payload[len(payload)-checksumSize:], checksum[:checksumSize])
	return prefix + encodeBase58(payload)
}

// TrackerHash returns the non-enumerable swarm identifier used for tracker discovery.
func (i Invite) TrackerHash() [TrackerHashSize]byte {
	derived := i.derive("bork/tracker/v1", TrackerHashSize)
	var hash [TrackerHashSize]byte
	copy(hash[:], derived)
	return hash
}

// GroupMediaKey returns the room-scoped symmetric key that seals realtime
// group datagrams. It is a group transport key, not forward-secret group E2EE:
// every RoomSeed holder can decrypt, while signatures bind packets to a
// transient NodeID for forwarding and replay checks, not member authorization.
func (i Invite) GroupMediaKey() [32]byte {
	derived := i.derive("bork/group-media/v1", 32)
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

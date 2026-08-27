package identity

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
)

const peerIDPrefix = "b2"

var peerIDEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// PeerID identifies one room membership. It is replaced on every join.
type PeerID [16]byte

func New() (PeerID, error) {
	var peerID PeerID
	for peerID.IsZero() {
		if _, err := rand.Read(peerID[:]); err != nil {
			return PeerID{}, fmt.Errorf("generate peer ID: %w", err)
		}
	}
	return peerID, nil
}

func ParsePeerID(value string) (PeerID, error) {
	if len(value) < len(peerIDPrefix) || value[:len(peerIDPrefix)] != peerIDPrefix {
		return PeerID{}, errors.New("invalid peer ID prefix")
	}
	decoded, err := peerIDEncoding.DecodeString(value[len(peerIDPrefix):])
	if err != nil || len(decoded) != len(PeerID{}) {
		return PeerID{}, errors.New("invalid peer ID")
	}
	var peerID PeerID
	copy(peerID[:], decoded)
	if peerID.IsZero() {
		return PeerID{}, errors.New("invalid peer ID")
	}
	if peerID.String() != value {
		return PeerID{}, errors.New("non-canonical peer ID")
	}
	return peerID, nil
}

func (id PeerID) String() string {
	return peerIDPrefix + peerIDEncoding.EncodeToString(id[:])
}

func (id PeerID) IsZero() bool {
	return id == PeerID{}
}

func (id PeerID) MarshalText() ([]byte, error) {
	return []byte(id.String()), nil
}

func (id *PeerID) UnmarshalText(text []byte) error {
	parsed, err := ParsePeerID(string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

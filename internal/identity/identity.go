package identity

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
)

const peerIDPrefix = "b1"

var peerIDEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// PeerID is the temporary Ed25519 public key used for one room membership.
type PeerID [ed25519.PublicKeySize]byte

type LocalIdentity struct {
	PeerID
	privateKey ed25519.PrivateKey
}

func New() (*LocalIdentity, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate room-membership key: %w", err)
	}
	return &LocalIdentity{PeerID: PeerID(publicKey), privateKey: privateKey}, nil
}

func ParsePeerID(value string) (PeerID, error) {
	if len(value) < len(peerIDPrefix) || value[:len(peerIDPrefix)] != peerIDPrefix {
		return PeerID{}, errors.New("invalid peer ID prefix")
	}
	decoded, err := peerIDEncoding.DecodeString(value[len(peerIDPrefix):])
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return PeerID{}, errors.New("invalid peer ID")
	}
	var peerID PeerID
	copy(peerID[:], decoded)
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

func (i *LocalIdentity) Public() crypto.PublicKey {
	return append(ed25519.PublicKey(nil), i.PeerID[:]...)
}

func (i *LocalIdentity) Sign(_ io.Reader, message []byte, options crypto.SignerOpts) ([]byte, error) {
	if options.HashFunc() != crypto.Hash(0) {
		return nil, errors.New("Ed25519 messages must not be pre-hashed")
	}
	return ed25519.Sign(i.privateKey, message), nil
}

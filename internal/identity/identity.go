package identity

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"strings"
)

type Identity struct {
	publicKey ed25519.PublicKey
	peerID    string
}

type LocalIdentity struct {
	Identity
	privateKey ed25519.PrivateKey
}

func New() (*LocalIdentity, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate room-membership node key: %w", err)
	}
	publicIdentity, err := FromPublicKey(publicKey)
	if err != nil {
		return nil, err
	}
	return &LocalIdentity{Identity: publicIdentity, privateKey: privateKey}, nil
}

func (i Identity) PeerID() string {
	return i.peerID
}

func (i Identity) PublicKey() ed25519.PublicKey {
	return append(ed25519.PublicKey(nil), i.publicKey...)
}

func (i *LocalIdentity) Public() crypto.PublicKey {
	return i.PublicKey()
}

func (i *LocalIdentity) Sign(_ io.Reader, message []byte, options crypto.SignerOpts) ([]byte, error) {
	if options.HashFunc() != crypto.Hash(0) {
		return nil, errors.New("Ed25519 messages must not be pre-hashed")
	}
	return ed25519.Sign(i.privateKey, message), nil
}

func FromPublicKey(publicKey ed25519.PublicKey) (Identity, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return Identity{}, errors.New("invalid Ed25519 public key")
	}
	publicKey = append(ed25519.PublicKey(nil), publicKey...)
	digest := sha256.Sum256(publicKey)
	encoded := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:]))
	return Identity{publicKey: publicKey, peerID: "b1" + encoded}, nil
}

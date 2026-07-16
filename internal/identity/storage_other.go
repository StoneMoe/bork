//go:build !windows

package identity

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
)

func protectSeed(seed []byte) ([]byte, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, errors.New("invalid Ed25519 seed")
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	protected := make([]byte, 0, ed25519.SeedSize+ed25519.PublicKeySize)
	protected = append(protected, seed...)
	return append(protected, publicKey...), nil
}

func isPublishConflict(err error) bool {
	return errors.Is(err, os.ErrExist)
}

func isRetryableAccessError(error) bool {
	return false
}

func unprotectSeed(contents []byte) ([]byte, error) {
	if len(contents) != ed25519.SeedSize+ed25519.PublicKeySize {
		return nil, errors.New("invalid identity data length")
	}
	seed := contents[:ed25519.SeedSize]
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	if !bytes.Equal(contents[ed25519.SeedSize:], publicKey) {
		return nil, errors.New("identity integrity check failed")
	}
	return append([]byte(nil), seed...), nil
}

func decodeStoredSeed(contents []byte) ([]byte, error) {
	if len(contents) <= len(identityMagic) || string(contents[:len(identityMagic)]) != identityMagic {
		return nil, errors.New("identity data is corrupt or unsupported")
	}
	return unprotectSeed(contents[len(identityMagic):])
}

func publishFile(source, destination string) error {
	if err := os.Link(source, destination); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(destination))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

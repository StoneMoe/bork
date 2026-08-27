package protocol

import (
	"bytes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

type SessionCiphers struct {
	Send    cipher.AEAD
	Receive cipher.AEAD
}

func DeriveSession(privateKey *ecdh.PrivateKey, localHello, remoteHello SessionHello) (SessionCiphers, error) {
	remotePublic, err := validateSessionInputs(privateKey, localHello, remoteHello)
	if err != nil {
		return SessionCiphers{}, err
	}
	sharedSecret, err := privateKey.ECDH(remotePublic)
	if err != nil {
		return SessionCiphers{}, fmt.Errorf("derive X25519 secret: %w", err)
	}

	transcriptHash, localFirst := sessionTranscript(localHello, remoteHello)

	packetsAB, err := deriveSessionCipher(sharedSecret, transcriptHash, wireDomain+"chacha20poly1305/session-packets/a-to-b")
	if err != nil {
		return SessionCiphers{}, err
	}
	packetsBA, err := deriveSessionCipher(sharedSecret, transcriptHash, wireDomain+"chacha20poly1305/session-packets/b-to-a")
	if err != nil {
		return SessionCiphers{}, err
	}
	if localFirst {
		return SessionCiphers{Send: packetsAB, Receive: packetsBA}, nil
	}
	return SessionCiphers{Send: packetsBA, Receive: packetsAB}, nil
}

func validateSessionInputs(privateKey *ecdh.PrivateKey, localHello, remoteHello SessionHello) (*ecdh.PublicKey, error) {
	if privateKey == nil {
		return nil, errors.New("local X25519 key is nil")
	}
	if !validSessionPeers(localHello, remoteHello) {
		return nil, errors.New("session peer IDs are invalid")
	}
	if !validSessionIDPair(localHello.SessionID, remoteHello.SessionID) {
		return nil, errors.New("session hello session IDs do not match")
	}
	if !bytes.Equal(privateKey.PublicKey().Bytes(), localHello.EphemeralKey[:]) {
		return nil, errors.New("local X25519 key does not match hello")
	}
	remotePublic, err := ecdh.X25519().NewPublicKey(remoteHello.EphemeralKey[:])
	if err != nil {
		return nil, fmt.Errorf("parse remote X25519 key: %w", err)
	}
	return remotePublic, nil
}

func validSessionPeers(localHello, remoteHello SessionHello) bool {
	return !localHello.PeerID.IsZero() && !remoteHello.PeerID.IsZero() && localHello.PeerID != remoteHello.PeerID
}

func validSessionIDPair(local, remote [16]byte) bool {
	return local != ([16]byte{}) && local == remote
}

func sessionTranscript(localHello, remoteHello SessionHello) ([32]byte, bool) {
	first, second := localHello, remoteHello
	localFirst := bytes.Compare(localHello.PeerID[:], remoteHello.PeerID[:]) < 0
	if !localFirst {
		first, second = second, first
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(wireDomain + "session-transcript\x00"))
	_, _ = hash.Write(first.wire[:])
	_, _ = hash.Write(second.wire[:])
	var transcriptHash [32]byte
	copy(transcriptHash[:], hash.Sum(nil))
	return transcriptHash, localFirst
}

func deriveSessionCipher(sharedSecret []byte, transcriptHash [32]byte, info string) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, sharedSecret, transcriptHash[:], info, chacha20poly1305.KeySize)
	if err != nil {
		return nil, err
	}
	return chacha20poly1305.New(key)
}

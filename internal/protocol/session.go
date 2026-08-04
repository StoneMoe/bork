package protocol

import (
	"bytes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

type SessionMaterial struct {
	SessionID [16]byte
	Ciphers   SessionCiphers
}

type SessionCiphers struct {
	ControlSend cipher.AEAD
	ControlRecv cipher.AEAD
}

func DeriveSession(privateKey *ecdh.PrivateKey, localHello, remoteHello HelloPacket) (SessionMaterial, error) {
	if privateKey == nil {
		return SessionMaterial{}, errors.New("local X25519 key is nil")
	}
	if len(localHello.IdentityKey) != 32 || len(remoteHello.IdentityKey) != 32 {
		return SessionMaterial{}, errors.New("session identity key is invalid")
	}
	if bytes.Equal(localHello.IdentityKey, remoteHello.IdentityKey) {
		return SessionMaterial{}, errors.New("session identities are equal")
	}
	if localHello.RoomTag != remoteHello.RoomTag {
		return SessionMaterial{}, errors.New("session room tags do not match")
	}
	if !bytes.Equal(privateKey.PublicKey().Bytes(), localHello.EphemeralKey[:]) {
		return SessionMaterial{}, errors.New("local X25519 key does not match hello")
	}
	remotePublic, err := ecdh.X25519().NewPublicKey(remoteHello.EphemeralKey[:])
	if err != nil {
		return SessionMaterial{}, fmt.Errorf("parse remote X25519 key: %w", err)
	}
	sharedSecret, err := privateKey.ECDH(remotePublic)
	if err != nil {
		return SessionMaterial{}, fmt.Errorf("derive X25519 secret: %w", err)
	}

	first, second := localHello, remoteHello
	localFirst := bytes.Compare(localHello.IdentityKey, remoteHello.IdentityKey) < 0
	if !localFirst {
		first, second = second, first
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(wireDomain + "handshake-transcript\x00"))
	var length [2]byte
	binary.BigEndian.PutUint16(length[:], uint16(len(first.wire)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write(first.wire[:])
	binary.BigEndian.PutUint16(length[:], uint16(len(second.wire)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write(second.wire[:])
	var transcriptHash [32]byte
	copy(transcriptHash[:], hash.Sum(nil))

	sessionID, err := hkdf.Key(sha256.New, sharedSecret, transcriptHash[:], wireDomain+"session-id", 16)
	if err != nil {
		return SessionMaterial{}, err
	}
	var material SessionMaterial
	copy(material.SessionID[:], sessionID)
	controlAB, err := deriveSessionCipher(sharedSecret, transcriptHash, wireDomain+"chacha20poly1305/control/a-to-b")
	if err != nil {
		return SessionMaterial{}, err
	}
	controlBA, err := deriveSessionCipher(sharedSecret, transcriptHash, wireDomain+"chacha20poly1305/control/b-to-a")
	if err != nil {
		return SessionMaterial{}, err
	}
	if localFirst {
		material.Ciphers = SessionCiphers{ControlSend: controlAB, ControlRecv: controlBA}
	} else {
		material.Ciphers = SessionCiphers{ControlSend: controlBA, ControlRecv: controlAB}
	}
	return material, nil
}

func deriveSessionCipher(sharedSecret []byte, transcriptHash [32]byte, info string) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, sharedSecret, transcriptHash[:], info, chacha20poly1305.KeySize)
	if err != nil {
		return nil, err
	}
	return chacha20poly1305.New(key)
}

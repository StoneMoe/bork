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

type LinkKeys struct {
	ControlSend [32]byte
	ControlRecv [32]byte
	VoiceSend   [32]byte
	VoiceRecv   [32]byte
}

type SessionMaterial struct {
	SessionID      [16]byte
	TranscriptHash [32]byte
	Keys           LinkKeys
}

type LinkCiphers struct {
	ControlSend cipher.AEAD
	ControlRecv cipher.AEAD
	VoiceSend   cipher.AEAD
	VoiceRecv   cipher.AEAD
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
	var material SessionMaterial
	copy(material.TranscriptHash[:], hash.Sum(nil))

	sessionID, err := hkdf.Key(sha256.New, sharedSecret, material.TranscriptHash[:], wireDomain+"session-id", 16)
	if err != nil {
		return SessionMaterial{}, err
	}
	copy(material.SessionID[:], sessionID)
	controlAB, err := deriveSessionKey(sharedSecret, material.TranscriptHash, wireDomain+"chacha20poly1305/control/a-to-b")
	if err != nil {
		return SessionMaterial{}, err
	}
	controlBA, err := deriveSessionKey(sharedSecret, material.TranscriptHash, wireDomain+"chacha20poly1305/control/b-to-a")
	if err != nil {
		return SessionMaterial{}, err
	}
	voiceAB, err := deriveSessionKey(sharedSecret, material.TranscriptHash, wireDomain+"chacha20poly1305/voice/a-to-b")
	if err != nil {
		return SessionMaterial{}, err
	}
	voiceBA, err := deriveSessionKey(sharedSecret, material.TranscriptHash, wireDomain+"chacha20poly1305/voice/b-to-a")
	if err != nil {
		return SessionMaterial{}, err
	}
	if localFirst {
		material.Keys = LinkKeys{ControlSend: controlAB, ControlRecv: controlBA, VoiceSend: voiceAB, VoiceRecv: voiceBA}
	} else {
		material.Keys = LinkKeys{ControlSend: controlBA, ControlRecv: controlAB, VoiceSend: voiceBA, VoiceRecv: voiceAB}
	}
	return material, nil
}

func deriveSessionKey(sharedSecret []byte, transcriptHash [32]byte, info string) ([32]byte, error) {
	derived, err := hkdf.Key(sha256.New, sharedSecret, transcriptHash[:], info, 32)
	if err != nil {
		return [32]byte{}, err
	}
	var key [32]byte
	copy(key[:], derived)
	return key, nil
}

func NewLinkCiphers(keys LinkKeys) (LinkCiphers, error) {
	controlSend, err := chacha20poly1305.New(keys.ControlSend[:])
	if err != nil {
		return LinkCiphers{}, err
	}
	controlRecv, err := chacha20poly1305.New(keys.ControlRecv[:])
	if err != nil {
		return LinkCiphers{}, err
	}
	voiceSend, err := chacha20poly1305.New(keys.VoiceSend[:])
	if err != nil {
		return LinkCiphers{}, err
	}
	voiceRecv, err := chacha20poly1305.New(keys.VoiceRecv[:])
	if err != nil {
		return LinkCiphers{}, err
	}
	return LinkCiphers{ControlSend: controlSend, ControlRecv: controlRecv, VoiceSend: voiceSend, VoiceRecv: voiceRecv}, nil
}

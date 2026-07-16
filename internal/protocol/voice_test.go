package protocol

import (
	"bytes"
	"testing"
)

func TestVoiceRoundTrip(t *testing.T) {
	pair := newTestSessionPair(t)
	payload := []byte{4, 5, 6}
	packet, err := MarshalVoice(pair.roomTag, pair.firstMaterial.SessionID, 42, 96000, payload, pair.firstCiphers.VoiceSend)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(packet[establishedHeaderSize:], payload) {
		t.Fatal("voice payload remained visible in ciphertext")
	}
	decoded, err := ParseVoice(packet, pair.roomTag, pair.secondMaterial.SessionID, pair.secondCiphers.VoiceRecv)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Sequence != 42 || decoded.Timestamp != 96000 || string(decoded.Payload) != string(payload) {
		t.Fatalf("decoded packet = %#v", decoded)
	}
}

func TestVoiceRejectsTamperingAndInvalidPayload(t *testing.T) {
	pair := newTestSessionPair(t)
	if _, err := MarshalVoice(pair.roomTag, pair.firstMaterial.SessionID, 1, 0, nil, pair.firstCiphers.VoiceSend); err == nil {
		t.Fatal("MarshalVoice() accepted empty payload")
	}
	if _, err := MarshalVoice(pair.roomTag, pair.firstMaterial.SessionID, 1, 0, make([]byte, MaxVoicePayload+1), pair.firstCiphers.VoiceSend); err == nil {
		t.Fatal("MarshalVoice() accepted oversized payload")
	}
	packet, err := MarshalVoice(pair.roomTag, pair.firstMaterial.SessionID, 1, 0, []byte{1, 2, 3}, pair.firstCiphers.VoiceSend)
	if err != nil {
		t.Fatal(err)
	}
	packet[establishedHeaderSize+voicePlaintextFixedSize] ^= 0xff
	if _, err := ParseVoice(packet, pair.roomTag, pair.firstMaterial.SessionID, pair.secondCiphers.VoiceRecv); err == nil {
		t.Fatal("ParseVoice() accepted tampered packet")
	}
}

func TestVoicePacketStaysWithinDatagramBudget(t *testing.T) {
	pair := newTestSessionPair(t)
	packet, err := MarshalVoice(pair.roomTag, pair.firstMaterial.SessionID, 1, 480, make([]byte, MaxVoicePayload), pair.firstCiphers.VoiceSend)
	if err != nil {
		t.Fatal(err)
	}
	if len(packet) != MaxVoicePacketSize {
		t.Fatalf("voice packet length = %d, want %d", len(packet), MaxVoicePacketSize)
	}
}

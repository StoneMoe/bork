package peer

import (
	"bytes"
	"crypto/cipher"
	"testing"

	"bork/internal/invite"
	"bork/internal/networking"
)

type recordingAEAD struct {
	cipher.AEAD
	nonces [][]byte
}

func (r *recordingAEAD) Seal(destination, nonce, plaintext, additionalData []byte) []byte {
	r.nonces = append(r.nonces, append([]byte(nil), nonce...))
	return r.AEAD.Seal(destination, nonce, plaintext, additionalData)
}

func TestScreenMediaDoesNotReuseNonceAcrossAudioAndVideo(t *testing.T) {
	roomInvite, err := invite.New("screen media nonce")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(roomInvite, networking.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	client.localScreenState = screenState{
		generation: 2, active: true, streamID: [16]byte{1},
		codec: ScreenVideoCodecH264Baseline, width: 640, height: 360,
	}
	recorder := &recordingAEAD{AEAD: client.roomDatagramProtector}
	client.roomDatagramProtector = recorder

	if _, err := client.sendScreenVideoChunk(1, 10_000, true, []byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := client.sendScreenAudioFrame(480, []byte{2}); err != nil {
		t.Fatal(err)
	}
	if len(recorder.nonces) != 2 || bytes.Equal(recorder.nonces[0], recorder.nonces[1]) {
		t.Fatalf("screen video and audio nonces = %x", recorder.nonces)
	}
}

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

	if _, err := client.sendScreenVideoChunk(1, 10_000, true, 640, 360, []byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := client.sendScreenAudioFrame(480, []byte{2}); err != nil {
		t.Fatal(err)
	}
	if len(recorder.nonces) != 2 || bytes.Equal(recorder.nonces[0], recorder.nonces[1]) {
		t.Fatalf("screen video and audio nonces = %x", recorder.nonces)
	}
}

func TestScreenVideoDisplaySizeMatchesCodedFrame(t *testing.T) {
	state := screenState{
		generation: 2, active: true, streamID: [16]byte{1},
		codec: ScreenVideoCodecH264Baseline, width: 640, height: 360,
	}
	metadata := screenVideoMetadata{
		generation: 2, codec: ScreenVideoCodecH264Baseline,
		displayWidth: 320, displayHeight: 360,
		timestamp: 1, duration: 10_000, keyFrame: true,
	}
	fragments, err := encodeScreenVideoFragments(metadata, []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	fragment, err := decodeScreenVideoFragment(fragments[0])
	if err != nil {
		t.Fatal(err)
	}
	if !screenVideoMetadataMatchesState(fragment.metadata, state) {
		t.Fatal("resized display should fit the coded frame")
	}

	fragment.metadata.displayWidth = 642
	if screenVideoMetadataMatchesState(fragment.metadata, state) {
		t.Fatal("display wider than the coded frame should be rejected")
	}
}

package peer

import (
	"bytes"
	"testing"
	"time"

	"bork/internal/invite"
	"bork/internal/networking"
)

func TestNewClientUsesEphemeralRoomIdentity(t *testing.T) {
	roomInvite, err := invite.New("test room")
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewClient(roomInvite, networking.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewClient(roomInvite, networking.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.PeerID() == second.PeerID() {
		t.Fatal("room clients reused an Ed25519 identity")
	}

	if err := first.rotateHelloEpoch(); err != nil {
		t.Fatal(err)
	}
	identityKey := append([]byte(nil), first.localHello.IdentityKey...)
	if err := first.rotateHelloEpoch(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(identityKey, first.localHello.IdentityKey) {
		t.Fatal("client changed its Ed25519 identity within one room membership")
	}
}

func TestExpireRemotePeersDropsStaleSessionRecord(t *testing.T) {
	now := time.Now()
	stale := now.Add(-remotePeerTimeout - time.Second)
	freshPending := &PeeringSession{lastAuthenticatedPacketAt: now}
	client := &Client{remotePeers: map[string]*RemotePeer{
		"gone": {
			activeSession: &PeeringSession{everAuthenticated: true, lastAuthenticatedPacketAt: stale},
		},
		"pending": {
			activeSession:  &PeeringSession{everAuthenticated: true, lastAuthenticatedPacketAt: stale},
			pendingSession: freshPending,
		},
	}}

	client.expireRemotePeers()

	if _, exists := client.remotePeers["gone"]; exists {
		t.Fatal("stale remote session record was retained")
	}
	peer := client.remotePeers["pending"]
	if peer == nil || peer.activeSession != nil || peer.pendingSession != freshPending {
		t.Fatal("fresh pending session was not preserved")
	}
}

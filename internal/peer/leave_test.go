package peer

import (
	"crypto/aes"
	"crypto/cipher"
	"net/netip"
	"testing"

	"bork/internal/identity"
	"bork/internal/invite"
	"bork/internal/networking"
	"bork/internal/protocol"
)

func TestHandleLeaveRemovesPeerDuringReconnect(t *testing.T) {
	roomInvite, err := invite.New("leave")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(roomInvite, networking.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	remoteIdentity, err := identity.New()
	if err != nil {
		t.Fatal(err)
	}
	path, err := NewPath(netip.MustParseAddrPort("192.0.2.20:4000"))
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	protector, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := [16]byte{2}
	client.remotePeers[remoteIdentity] = &RemotePeer{
		peerID: remoteIdentity,
		activeSession: &Session{
			path: path, localHello: protocol.SessionHello{SessionID: sessionID}, everAuthenticated: true,
			ciphers: protocol.SessionCiphers{Send: protector, Receive: protector},
		},
	}
	packet, err := protocol.MarshalControl(protocol.PacketLeave, sessionID, 1, 0, protector)
	if err != nil {
		t.Fatal(err)
	}

	client.handleLeavePacketOnPath(packet, path)

	if _, exists := client.remotePeers[remoteIdentity]; exists {
		t.Fatal("authenticated Leave did not remove the remote peer")
	}
	select {
	case <-client.stateChanges:
	default:
		t.Fatal("authenticated Leave did not publish a state change")
	}
}

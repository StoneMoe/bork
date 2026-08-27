package peer

import (
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"bork/internal/identity"
	"bork/internal/invite"
	"bork/internal/networking"
	"bork/internal/networking/discovery"
)

func TestNewClientUsesEphemeralRoomPeerID(t *testing.T) {
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
		t.Fatal("room clients reused a peer ID")
	}
}

func TestExpireRemotePeersDropsStaleSessionRecord(t *testing.T) {
	now := time.Now()
	stale := now.Add(-remotePeerTimeout - time.Second)
	freshPending := &Session{lastAuthenticatedPacketAt: now}
	goneID := identity.PeerID{1}
	pendingID := identity.PeerID{2}
	client := &Client{remotePeers: map[identity.PeerID]*RemotePeer{
		goneID: {
			peerID:        goneID,
			activeSession: &Session{everAuthenticated: true, lastAuthenticatedPacketAt: stale},
		},
		pendingID: {
			peerID:         pendingID,
			activeSession:  &Session{everAuthenticated: true, lastAuthenticatedPacketAt: stale},
			pendingSession: freshPending,
		},
	}, stateChanges: make(chan struct{}, 1)}

	client.expireRemotePeers()

	if _, exists := client.remotePeers[goneID]; exists {
		t.Fatal("stale remote session record was retained")
	}
	peer := client.remotePeers[pendingID]
	if peer == nil || peer.activeSession != nil || peer.pendingSession != freshPending {
		t.Fatal("fresh pending session was not preserved")
	}
	select {
	case <-client.stateChanges:
	default:
		t.Fatal("removing the visible stale session did not publish a state change")
	}
}

func TestExpireRemotePeersKeepsVisiblePeerWhileReconnecting(t *testing.T) {
	session := &Session{
		authenticated:             true,
		everAuthenticated:         true,
		lastAuthenticatedPacketAt: time.Now().Add(-pathFailoverTimeout - time.Second),
	}
	client := &Client{
		logger: slog.New(slog.DiscardHandler),
		remotePeers: map[identity.PeerID]*RemotePeer{
			{1}: {peerID: identity.PeerID{1}, activeSession: session},
		},
	}

	client.expireRemotePeers()

	if session.authenticated {
		t.Fatal("stale transport remained authenticated")
	}
	snapshots := client.remotePeerSnapshots()
	if len(snapshots) != 1 || snapshots[0].Connected {
		t.Fatalf("reconnecting peer snapshot = %+v", snapshots)
	}
}

func TestHistoricalRemoteHintRefreshesExpiry(t *testing.T) {
	now := time.Date(2026, time.August, 15, 12, 0, 0, 0, time.UTC)
	address := netip.MustParseAddrPort("203.0.113.10:4000")
	path := Path{address: address}
	client := &Client{}
	client.rememberAuthenticatedPath(path, now)
	first := client.discoveredAddresses[address]
	if first.source != discovery.SourceHistoricalRemote || first.expiresAt != now.Add(knownPeerTTL) {
		t.Fatalf("historical remote hint = %+v", first)
	}

	renewedAt := now.Add(knownPeerTTL / 2)
	client.rememberAuthenticatedPath(path, renewedAt)
	if client.expireDiscoveryHints(first.expiresAt) {
		t.Fatal("renewed historical remote hint expired at its old deadline")
	}
	renewed := client.discoveredAddresses[address]
	if renewed.expiresAt != renewedAt.Add(knownPeerTTL) {
		t.Fatalf("renewed expiry = %v", renewed.expiresAt)
	}
	if !client.expireDiscoveryHints(renewed.expiresAt) {
		t.Fatal("historical remote hint did not expire")
	}
}

func TestHistoricalRemoteHintKeepsRoomLifetimeSource(t *testing.T) {
	now := time.Date(2026, time.August, 15, 12, 0, 0, 0, time.UTC)
	address := netip.MustParseAddrPort("192.0.2.10:4000")
	path := Path{address: address}
	client := &Client{discoveredAddresses: map[netip.AddrPort]discoveredAddress{
		address: {source: discovery.SourceMDNS},
	}}
	client.rememberAuthenticatedPath(path, now)
	remembered := client.discoveredAddresses[address]
	if remembered.source != discovery.SourceMDNS || !remembered.expiresAt.IsZero() {
		t.Fatalf("room-lifetime source was replaced: %+v", remembered)
	}
}

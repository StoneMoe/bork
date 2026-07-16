package peer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"sync"
	"testing"
	"time"

	"bork/internal/identity"
	"bork/internal/invite"
	"bork/internal/networking"
	"bork/internal/networking/endpoint"
)

type fakeRoomNetwork struct {
	mu              sync.RWMutex
	snapshot        networking.RoomSnapshot
	snapshotChanges chan struct{}
	packets         chan endpoint.Datagram
	discovered      chan netip.AddrPort
	done            chan struct{}
	err             error
}

func newFakeRoomNetwork() *fakeRoomNetwork {
	return &fakeRoomNetwork{snapshotChanges: make(chan struct{}, 1), packets: make(chan endpoint.Datagram, 4), discovered: make(chan netip.AddrPort, 4), done: make(chan struct{})}
}

func (e *fakeRoomNetwork) Run(ctx context.Context) error {
	defer close(e.done)
	if e.err != nil {
		return e.err
	}
	<-ctx.Done()
	return nil
}

func (e *fakeRoomNetwork) Snapshot() networking.RoomSnapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.snapshot
}
func (e *fakeRoomNetwork) StateChanges() <-chan struct{}            { return e.snapshotChanges }
func (e *fakeRoomNetwork) DiscoveredPeers() <-chan netip.AddrPort   { return e.discovered }
func (e *fakeRoomNetwork) ControlPackets() <-chan endpoint.Datagram { return e.packets }
func (e *fakeRoomNetwork) VoicePackets() <-chan endpoint.Datagram   { return nil }
func (e *fakeRoomNetwork) SendControl([]byte, netip.AddrPort) error { return nil }
func (e *fakeRoomNetwork) SendVoiceBatch(endpoint.VoiceBatch) error { return nil }
func (e *fakeRoomNetwork) InvalidateVoice(uint64)                   {}

func (e *fakeRoomNetwork) updateSnapshot(snapshot networking.RoomSnapshot) {
	e.mu.Lock()
	e.snapshot = snapshot
	e.mu.Unlock()
	select {
	case e.snapshotChanges <- struct{}{}:
	default:
	}
}

func TestClientLoopStopsWithContext(t *testing.T) {
	client := testClient(t, func() roomNetwork { return newFakeRoomNetwork() })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Loop(ctx, nil) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not stop after context cancellation")
	}
}

func TestClientAppliesNetworkSnapshot(t *testing.T) {
	createdNetwork := make(chan *fakeRoomNetwork, 1)
	client := testClient(t, func() roomNetwork {
		network := newFakeRoomNetwork()
		createdNetwork <- network
		return network
	})
	peerChanges := client.StateChanges()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Loop(ctx, nil) }()
	network := <-createdNetwork
	network.updateSnapshot(networking.RoomSnapshot{Endpoint: endpoint.Snapshot{
		ListenAddress: "[::]:7778",
		Candidates:    []endpoint.Candidate{{Type: endpoint.CandidateHost, Address: "192.0.2.10:7778", Family: "ipv4"}},
	}})
	waitForClientState(t, client, peerChanges, func(snapshot ClientSnapshot, diagnostics networking.RoomSnapshot) bool {
		return snapshot.Phase == "discovering" && len(diagnostics.Endpoint.Candidates) == 1
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestClientReportsNetworkFailure(t *testing.T) {
	client := testClient(t, func() roomNetwork {
		network := newFakeRoomNetwork()
		network.err = errors.New("bind failed")
		return network
	})
	done := make(chan error, 1)
	go func() { done <- client.Loop(context.Background(), nil) }()
	if err := <-done; err == nil || err.Error() != "bind failed" {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestClientLoopIsOneShot(t *testing.T) {
	client := testClient(t, func() roomNetwork { return newFakeRoomNetwork() })
	client.started.Store(true)
	if err := client.Loop(context.Background(), nil); err == nil {
		t.Fatal("second Client.Loop() call was accepted")
	}
}

func testClient(t *testing.T, factory roomNetworkFactory) *Client {
	t.Helper()
	device, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomInvite, err := invite.New("Room")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newClient(device, roomInvite, factory, logger)
}

func waitForClientState(t *testing.T, client *Client, peerChanges <-chan struct{}, condition func(ClientSnapshot, networking.RoomSnapshot) bool) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		if condition(client.Snapshot(), client.NetworkSnapshot()) {
			return
		}
		select {
		case <-peerChanges:
		case <-deadline:
			t.Fatalf("timed out waiting for client state: %#v %#v", client.Snapshot(), client.NetworkSnapshot())
		}
	}
}

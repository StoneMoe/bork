package peer

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bork/internal/identity"
	"bork/internal/invite"
	"bork/internal/networking"
	"bork/internal/networking/discovery"
	discoverytracker "bork/internal/networking/discovery/tracker"
	"bork/internal/networking/endpoint"
)

type restrictedNATHub struct {
	mu      sync.RWMutex
	peers   map[netip.AddrPort]*internetTestNetwork
	opened  map[netip.AddrPort]map[netip.AddrPort]bool
	dropped atomic.Int32
}

type internetTestNetwork struct {
	hub             *restrictedNATHub
	address         netip.AddrPort
	trackerURL      string
	trackerHash     [20]byte
	trackerIdentity [32]byte
	logger          *slog.Logger
	changes         chan struct{}
	discovered      chan discovery.Hint
	control         chan endpoint.Datagram
	snapshot        networking.RoomSnapshot
}

func newInternetTestNetwork(hub *restrictedNATHub, address netip.AddrPort, trackerURL string, trackerHash [20]byte, trackerIdentity [32]byte, logger *slog.Logger) *internetTestNetwork {
	network := &internetTestNetwork{
		hub: hub, address: address, trackerURL: trackerURL, trackerHash: trackerHash, trackerIdentity: trackerIdentity, logger: logger,
		changes: make(chan struct{}, 1), discovered: make(chan discovery.Hint, 64), control: make(chan endpoint.Datagram, 256),
		snapshot: networking.RoomSnapshot{Endpoint: endpoint.Snapshot{
			ListenAddress: address.String(),
			Candidates:    []endpoint.Candidate{{Type: endpoint.CandidateNIC, Address: address.String(), Family: "ipv4"}},
		}},
	}
	hub.mu.Lock()
	hub.peers[address] = network
	hub.mu.Unlock()
	return network
}

func (n *internetTestNetwork) Run(ctx context.Context) error {
	announcer, err := discoverytracker.New([]string{n.trackerURL}, n.trackerHash, n.trackerIdentity, nil, n.logger)
	if err != nil {
		return err
	}
	announcer.UpdateCandidates([]discoverytracker.AnnounceCandidate{{Address: n.address.Addr(), Port: n.address.Port()}})
	select {
	case n.changes <- struct{}{}:
	default:
	}
	return announcer.Run(ctx, n.discovered)
}

func (n *internetTestNetwork) Snapshot() networking.RoomSnapshot              { return n.snapshot }
func (n *internetTestNetwork) StateChanges() <-chan struct{}                  { return n.changes }
func (n *internetTestNetwork) DiscoveredPeers() <-chan discovery.Hint         { return n.discovered }
func (n *internetTestNetwork) ControlPackets() <-chan endpoint.Datagram       { return n.control }
func (n *internetTestNetwork) AudioPackets() <-chan endpoint.Datagram         { return nil }
func (n *internetTestNetwork) InteractivePackets() <-chan endpoint.Datagram   { return nil }
func (n *internetTestNetwork) SendRealtimeBatch(endpoint.RealtimeBatch) error { return nil }
func (n *internetTestNetwork) InvalidateRealtime(uint64)                      {}
func (n *internetTestNetwork) EnqueueControl(packet []byte, destination netip.AddrPort) error {
	n.hub.mu.Lock()
	if n.hub.opened[n.address] == nil {
		n.hub.opened[n.address] = make(map[netip.AddrPort]bool)
	}
	n.hub.opened[n.address][destination] = true
	allowed := n.hub.opened[destination][n.address]
	receiver := n.hub.peers[destination]
	n.hub.mu.Unlock()
	if receiver == nil || !allowed {
		n.hub.dropped.Add(1)
		return nil
	}
	receiver.control <- endpoint.Datagram{Data: append([]byte(nil), packet...), From: n.address, ReceivedAt: time.Now()}
	return nil
}

func (n *internetTestNetwork) EnqueueBackground(packet []byte, destination netip.AddrPort) error {
	return n.EnqueueControl(packet, destination)
}

func TestInternetStartupPunchesRestrictedNATAfterLatePeerAnnounce(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	room, err := invite.New("internet startup")
	if err != nil {
		t.Fatal(err)
	}
	addresses := []netip.AddrPort{
		netip.MustParseAddrPort("198.51.100.41:44101"),
		netip.MustParseAddrPort("198.51.100.42:44102"),
	}
	var trackerMu sync.Mutex
	trackerPeers := make(map[string]netip.AddrPort)
	requests := make(chan struct{}, 32)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		peerID := request.URL.Query().Get("peer_id")
		port, _ := strconv.ParseUint(request.URL.Query().Get("port"), 10, 16)
		trackerMu.Lock()
		if _, exists := trackerPeers[peerID]; !exists {
			trackerPeers[peerID] = addresses[len(trackerPeers)]
		}
		peer := trackerPeers[peerID]
		trackerPeers[peerID] = netip.AddrPortFrom(peer.Addr(), uint16(port))
		peers := make([]netip.AddrPort, 0, len(trackerPeers))
		for _, tracked := range trackerPeers {
			peers = append(peers, tracked)
		}
		sort.Slice(peers, func(i, j int) bool { return peers[i].Compare(peers[j]) < 0 })
		trackerMu.Unlock()
		select {
		case requests <- struct{}{}:
		default:
		}
		_, _ = writer.Write(compactHTTPTrackerResponse(3600, peers))
	}))
	defer server.Close()

	hub := &restrictedNATHub{peers: make(map[netip.AddrPort]*internetTestNetwork), opened: make(map[netip.AddrPort]map[netip.AddrPort]bool)}
	identities := make([]*identity.LocalIdentity, 2)
	clients := make([]*Client, 2)
	networks := make([]*internetTestNetwork, 2)
	for index := range clients {
		identities[index], err = identity.LoadOrCreate(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		var trackerIdentity [32]byte
		copy(trackerIdentity[:], identities[index].PublicKey())
		networks[index] = newInternetTestNetwork(hub, addresses[index], server.URL+"/announce", room.TrackerHash(), trackerIdentity, logger)
		network := networks[index]
		clients[index] = newClient(identities[index], room, func() roomNetwork { return network }, logger)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstDone := make(chan error, 1)
	go func() { firstDone <- clients[0].Loop(ctx, nil) }()
	select {
	case <-requests:
	case <-time.After(2 * time.Second):
		t.Fatal("first peer did not announce")
	}
	secondDone := make(chan error, 1)
	go func() { secondDone <- clients[1].Loop(ctx, nil) }()
	waitForInternetPeers(t, clients, identities, 10*time.Second)
	if hub.dropped.Load() == 0 {
		t.Fatal("restricted NAT simulation did not drop the initial one-way Hello")
	}
	cancel()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func waitForInternetPeers(t *testing.T, clients []*Client, identities []*identity.LocalIdentity, timeout time.Duration) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		connected := true
		for index, client := range clients {
			other := identities[1-index].PeerID()
			found := false
			snapshot, _ := client.StateSnapshot()
			for _, peer := range snapshot.RemotePeers {
				found = found || peer.PeerID == other
			}
			connected = connected && found
		}
		if connected {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("internet peers did not connect through restricted NAT")
		}
	}
}

func compactHTTPTrackerResponse(interval int, peers []netip.AddrPort) []byte {
	compact := make([]byte, 0, len(peers)*6)
	for _, peer := range peers {
		address := peer.Addr().Unmap().As4()
		compact = append(compact, address[:]...)
		compact = append(compact, byte(peer.Port()>>8), byte(peer.Port()))
	}
	response := []byte(fmt.Sprintf("d8:intervali%de5:peers%d:", interval, len(compact)))
	response = append(response, compact...)
	return append(response, 'e')
}

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
	"bork/internal/networking/discovery"
	"bork/internal/networking/discovery/tracker"
	"bork/internal/networking/endpoint"
)

type fakeRoomNetwork struct {
	mu              sync.RWMutex
	snapshot        networking.RoomSnapshot
	snapshotChanges chan struct{}
	packets         chan endpoint.Datagram
	discovered      chan discovery.Hint
	sent            []netip.AddrPort
	sentPackets     [][]byte
	background      [][]byte
	realtimeBatches []endpoint.RealtimeBatch
	enqueueErrors   map[netip.AddrPort]error
	done            chan struct{}
	err             error
}

type loadedRoomNetwork struct {
	*fakeRoomNetwork
	control chan endpoint.Datagram
	audio   chan endpoint.Datagram
}

func (n *loadedRoomNetwork) ControlPackets() <-chan endpoint.Datagram { return n.control }
func (n *loadedRoomNetwork) AudioPackets() <-chan endpoint.Datagram   { return n.audio }

func newFakeRoomNetwork() *fakeRoomNetwork {
	return &fakeRoomNetwork{
		snapshotChanges: make(chan struct{}, 1), packets: make(chan endpoint.Datagram, 4),
		discovered: make(chan discovery.Hint, 4), enqueueErrors: make(map[netip.AddrPort]error), done: make(chan struct{}),
	}
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
func (e *fakeRoomNetwork) StateChanges() <-chan struct{}                { return e.snapshotChanges }
func (e *fakeRoomNetwork) DiscoveredPeers() <-chan discovery.Hint       { return e.discovered }
func (e *fakeRoomNetwork) ControlPackets() <-chan endpoint.Datagram     { return e.packets }
func (e *fakeRoomNetwork) ReliablePackets() <-chan endpoint.Datagram    { return e.packets }
func (e *fakeRoomNetwork) BridgePackets() <-chan endpoint.Datagram      { return e.packets }
func (e *fakeRoomNetwork) AudioPackets() <-chan endpoint.Datagram       { return nil }
func (e *fakeRoomNetwork) InteractivePackets() <-chan endpoint.Datagram { return nil }
func (e *fakeRoomNetwork) EnqueueControl(packet []byte, destination netip.AddrPort) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.enqueueErrors[destination]; err != nil {
		return err
	}
	e.sent = append(e.sent, destination)
	e.sentPackets = append(e.sentPackets, append([]byte(nil), packet...))
	return nil
}
func (e *fakeRoomNetwork) EnqueueBackground(packet []byte, destination netip.AddrPort) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.enqueueErrors[destination]; err != nil {
		return err
	}
	e.background = append(e.background, append([]byte(nil), packet...))
	e.sent = append(e.sent, destination)
	e.sentPackets = append(e.sentPackets, append([]byte(nil), packet...))
	return nil
}
func (e *fakeRoomNetwork) SendRealtimeBatch(batch endpoint.RealtimeBatch) error {
	e.mu.Lock()
	e.realtimeBatches = append(e.realtimeBatches, batch)
	e.mu.Unlock()
	return nil
}
func (e *fakeRoomNetwork) InvalidateRealtime(uint64) {}

func (e *fakeRoomNetwork) sentAddresses() []netip.AddrPort {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return append([]netip.AddrPort(nil), e.sent...)
}

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
	if err := client.Loop(context.Background(), nil); err == nil {
		t.Fatal("second Client.Loop() call was accepted")
	}
}

func TestClientLoopDoesNotStarveCommandsUnderRealtimeLoad(t *testing.T) {
	network := &loadedRoomNetwork{
		fakeRoomNetwork: newFakeRoomNetwork(),
		control:         make(chan endpoint.Datagram, 256),
		audio:           make(chan endpoint.Datagram, 256),
	}
	packet := endpoint.Datagram{Data: []byte{0}}
	for range cap(network.control) {
		network.control <- packet
		network.audio <- packet
	}
	loadContext, stopLoad := context.WithCancel(context.Background())
	var flooders sync.WaitGroup
	fill := func(queue chan<- endpoint.Datagram) {
		defer flooders.Done()
		for {
			select {
			case queue <- packet:
			case <-loadContext.Done():
				return
			}
		}
	}
	flooders.Add(2)
	go fill(network.control)
	go fill(network.audio)

	client := testClient(t, func() roomNetwork { return network })
	loopContext, stopLoop := context.WithCancel(context.Background())
	loopDone := make(chan error, 1)
	go func() { loopDone <- client.Loop(loopContext, nil) }()
	<-client.Ready()
	commandDone := make(chan error, 1)
	go func() {
		_, err := client.OfferFile("missing", "missing")
		commandDone <- err
	}()

	var commandErr error
	timedOut := false
	select {
	case commandErr = <-commandDone:
	case <-time.After(time.Second):
		timedOut = true
	}
	stopLoad()
	flooders.Wait()
	stopLoop()
	select {
	case err := <-loopDone:
		if err != nil {
			t.Fatalf("Loop() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Loop() did not stop")
	}
	if timedOut {
		t.Fatal("file command was starved by realtime and control traffic")
	}
	if commandErr == nil {
		t.Fatal("invalid file command unexpectedly succeeded")
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
		Candidates:    []endpoint.Candidate{{Type: endpoint.CandidateNIC, Address: "192.0.2.10:7778", Family: "ipv4"}},
	}})
	waitForClientState(t, client, peerChanges, func(snapshot ClientSnapshot, diagnostics networking.RoomSnapshot) bool {
		return snapshot.Phase == "discovering" && len(diagnostics.Endpoint.Candidates) == 1
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestNetworkDiagnosticsDoNotInvalidateTopologyOrFanout(t *testing.T) {
	client := testClient(t, func() roomNetwork { return newFakeRoomNetwork() })
	base := networking.RoomSnapshot{Endpoint: endpoint.Snapshot{
		ListenAddress: "[::]:7778",
		Candidates:    []endpoint.Candidate{{Type: endpoint.CandidateNIC, Address: "192.0.2.10:7778", Family: "ipv4"}},
	}}
	client.networkSnapshot = base
	client.topologyGeneration = 7
	client.fanoutDirty = false

	diagnostics := base
	diagnostics.Endpoint.ListenAddress = "[::]:9000"
	diagnostics.Endpoint.STUN = []endpoint.STUNResult{{Server: "stun.example:3478", Error: "timeout"}}
	diagnostics.Tracker = []tracker.ProviderStatus{{Provider: "tracker.example"}}
	diagnostics.NetworkError = "network diagnostic"
	diagnostics.DiscoveryError = "discovery diagnostic"
	diagnostics.PortMappingError = "mapping diagnostic"
	client.applyNetworkSnapshot(diagnostics)
	if client.topologyGeneration != 7 || client.fanoutDirty {
		t.Fatalf("diagnostic snapshot invalidated topology: generation=%d fanoutDirty=%t", client.topologyGeneration, client.fanoutDirty)
	}
	_, projected := client.StateSnapshot()
	if projected.NetworkError != diagnostics.NetworkError || projected.DiscoveryError != diagnostics.DiscoveryError ||
		projected.PortMappingError != diagnostics.PortMappingError || len(projected.Endpoint.STUN) != 1 || len(projected.Tracker) != 1 {
		t.Fatalf("diagnostics were not projected: %#v", projected)
	}

	diagnostics.Endpoint.Candidates = append(diagnostics.Endpoint.Candidates, endpoint.Candidate{
		Type: endpoint.CandidateSTUN, Address: "198.51.100.10:7778", Family: "ipv4",
	})
	client.applyNetworkSnapshot(diagnostics)
	if client.topologyGeneration != 8 || client.fanoutDirty {
		t.Fatalf("candidate change invalidation = generation %d, fanoutDirty %t", client.topologyGeneration, client.fanoutDirty)
	}
}

func TestConnectivitySnapshotReportsDiscoveryHints(t *testing.T) {
	client, _, _ := testHelloClient(t)
	address := netip.MustParseAddrPort("198.51.100.80:48000")
	client.addDiscoveryHintAt(discovery.Hint{Address: address, Source: discovery.SourceTracker, ExpiresAt: time.Now().Add(time.Minute)}, time.Now())
	snapshot, _ := client.StateSnapshot()
	discoverySnapshot := snapshot.Connectivity
	if len(discoverySnapshot.DiscoveryHints) != 1 || discoverySnapshot.DiscoveryHints[0].Address != address.String() || discoverySnapshot.DiscoveryHints[0].Source != string(discovery.SourceTracker) {
		t.Fatalf("discovery diagnostics = %+v", discoverySnapshot)
	}
}

func TestStateSnapshotDeepCopiesSlices(t *testing.T) {
	client := testClient(t, func() roomNetwork { return newFakeRoomNetwork() })
	client.snapshot = ClientSnapshot{
		RemotePeers:  []RemotePeerSnapshot{{PeerID: "remote"}},
		Connectivity: ConnectivitySnapshot{DiscoveryHints: []DiscoveryHintSnapshot{{Address: "192.0.2.1:9000"}}},
	}
	client.networkSnapshot = networking.RoomSnapshot{
		Endpoint: endpoint.Snapshot{
			Candidates: []endpoint.Candidate{{Address: "192.0.2.2:9000"}},
			STUN:       []endpoint.STUNResult{{Server: "stun.example:3478"}},
		},
		Tracker: []tracker.ProviderStatus{{Provider: "tracker.example"}},
	}

	snapshot, networkSnapshot := client.StateSnapshot()
	snapshot.RemotePeers[0].PeerID = "changed"
	snapshot.Connectivity.DiscoveryHints[0].Address = "changed"
	networkSnapshot.Endpoint.Candidates[0].Address = "changed"
	networkSnapshot.Endpoint.STUN[0].Server = "changed"
	networkSnapshot.Tracker[0].Provider = "changed"

	unchanged, unchangedNetwork := client.StateSnapshot()
	if unchanged.RemotePeers[0].PeerID != "remote" || unchanged.Connectivity.DiscoveryHints[0].Address != "192.0.2.1:9000" ||
		unchangedNetwork.Endpoint.Candidates[0].Address != "192.0.2.2:9000" || unchangedNetwork.Endpoint.STUN[0].Server != "stun.example:3478" ||
		unchangedNetwork.Tracker[0].Provider != "tracker.example" {
		t.Fatalf("snapshot mutation reached client state: %#v %#v", unchanged, unchangedNetwork)
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

func TestDiscoveryHintProbingUsesImmediateAndBoundedBackoff(t *testing.T) {
	network := newFakeRoomNetwork()
	client := newDiscoveryTestClient(network)
	address := netip.MustParseAddrPort("203.0.113.10:9000")
	startedAt := time.Unix(1000, 0)
	client.addDiscoveryHintAt(discovery.Hint{
		Address:   address,
		Source:    discovery.SourceTracker,
		ExpiresAt: startedAt.Add(24 * time.Hour),
	}, startedAt)
	if sent := network.sentAddresses(); len(sent) != 1 || sent[0] != address {
		t.Fatalf("immediate Hello destinations = %v, want [%s]", sent, address)
	}
	if got := client.discoveredAddresses[address].probeInterval; got != helloInterval {
		t.Fatalf("initial probe interval = %v, want %v", got, helloInterval)
	}

	steps := []struct {
		at       time.Duration
		interval time.Duration
	}{
		{at: 2 * time.Second, interval: 4 * time.Second},
		{at: 6 * time.Second, interval: 8 * time.Second},
		{at: 14 * time.Second, interval: 8 * time.Second},
		{at: 22 * time.Second, interval: 8 * time.Second},
		{at: 30 * time.Second, interval: 8 * time.Second},
		{at: 38 * time.Second, interval: 8 * time.Second},
	}
	for index, step := range steps {
		client.sendHellos(startedAt.Add(step.at - time.Nanosecond))
		if got := len(network.sentAddresses()); got != index+1 {
			t.Fatalf("sent %d Hellos before probe %d was due, want %d", got, index+1, index+1)
		}
		client.sendHellos(startedAt.Add(step.at))
		if got := len(network.sentAddresses()); got != index+2 {
			t.Fatalf("sent %d Hellos after probe %d, want %d", got, index+1, index+2)
		}
		if got := client.discoveredAddresses[address].probeInterval; got != step.interval {
			t.Fatalf("probe %d interval = %v, want %v", index+1, got, step.interval)
		}
	}
}

func TestDiscoveryHintRefreshDoesNotResetProbeBackoff(t *testing.T) {
	network := newFakeRoomNetwork()
	client := newDiscoveryTestClient(network)
	address := netip.MustParseAddrPort("203.0.113.11:9000")
	startedAt := time.Unix(2000, 0)
	client.addDiscoveryHintAt(discovery.Hint{
		Address:   address,
		Source:    discovery.SourceTracker,
		ExpiresAt: startedAt.Add(time.Hour),
	}, startedAt)
	client.sendHellos(startedAt.Add(helloInterval))
	before := client.discoveredAddresses[address]
	refreshedAt := startedAt.Add(3 * time.Second)
	refreshedExpiry := startedAt.Add(2 * time.Hour)
	client.addDiscoveryHintAt(discovery.Hint{
		Address:   address,
		Source:    discovery.SourceTracker,
		ExpiresAt: refreshedExpiry,
	}, refreshedAt)
	after := client.discoveredAddresses[address]
	if len(network.sentAddresses()) != 2 {
		t.Fatal("a repeated hint sent an immediate Hello")
	}
	if !after.lastSeen.Equal(refreshedAt) || !after.expiresAt.Equal(refreshedExpiry) {
		t.Fatalf("refreshed record timestamps = %#v", after)
	}
	if after.probeInterval != before.probeInterval || !after.nextProbe.Equal(before.nextProbe) {
		t.Fatalf("refresh reset probe backoff: before=%#v after=%#v", before, after)
	}
	client.sendHellos(before.nextProbe.Add(-time.Nanosecond))
	if len(network.sentAddresses()) != 2 {
		t.Fatal("refreshed hint probed before the existing deadline")
	}
	client.sendHellos(before.nextProbe)
	if len(network.sentAddresses()) != 3 {
		t.Fatal("refreshed hint did not probe at the existing deadline")
	}
}

func TestDiscoveryHintRefreshAndExpirationPublishCurrentSnapshot(t *testing.T) {
	network := newFakeRoomNetwork()
	client := newDiscoveryTestClient(network)
	client.stateChanges = make(chan struct{}, 4)
	address := netip.MustParseAddrPort("203.0.113.15:9000")
	startedAt := time.Unix(2500, 0)
	client.addDiscoveryHintAt(discovery.Hint{
		Address: address, Source: discovery.SourceTracker, ExpiresAt: startedAt.Add(time.Hour),
	}, startedAt)
	if len(client.stateChanges) != 1 {
		t.Fatal("new hint did not publish exactly once")
	}
	<-client.stateChanges

	refreshedExpiry := startedAt.Add(2 * time.Hour)
	client.addDiscoveryHintAt(discovery.Hint{
		Address: address, Source: discovery.SourceTopology, ExpiresAt: refreshedExpiry,
	}, startedAt.Add(time.Minute))
	if len(client.stateChanges) != 1 || len(network.sentAddresses()) != 1 {
		t.Fatal("refreshed hint did not publish once or sent another immediate Hello")
	}
	snapshot, _ := client.StateSnapshot()
	if len(snapshot.Connectivity.DiscoveryHints) != 1 || snapshot.Connectivity.DiscoveryHints[0].Source != string(discovery.SourceTopology) ||
		snapshot.Connectivity.DiscoveryHints[0].ExpiresAt != refreshedExpiry.UTC().Format(time.RFC3339) {
		t.Fatalf("refreshed hint was stale in snapshot: %#v", snapshot.Connectivity.DiscoveryHints)
	}
	<-client.stateChanges

	if !client.expireDiscoveryHints(refreshedExpiry) {
		t.Fatal("expiration did not report a state change")
	}
	client.publishStateChange()
	snapshot, _ = client.StateSnapshot()
	if len(client.stateChanges) != 1 || len(snapshot.Connectivity.DiscoveryHints) != 0 {
		t.Fatalf("expired hint was stale in snapshot: %#v", snapshot.Connectivity.DiscoveryHints)
	}
}

func TestDiscoveryHintExpiryRemovesOnlyExpiredRecords(t *testing.T) {
	client := newDiscoveryTestClient(newFakeRoomNetwork())
	startedAt := time.Unix(3000, 0)
	trackerAddress := netip.MustParseAddrPort("203.0.113.12:9000")
	localAddress := netip.MustParseAddrPort("192.168.1.12:9000")
	client.rememberDiscoveryHint(discovery.Hint{
		Address:   trackerAddress,
		Source:    discovery.SourceTracker,
		ExpiresAt: startedAt.Add(time.Minute),
	}, startedAt)
	client.rememberDiscoveryHint(discovery.Hint{Address: localAddress, Source: discovery.SourceLocal}, startedAt)
	client.expireDiscoveryHints(startedAt.Add(time.Minute))
	if _, exists := client.discoveredAddresses[trackerAddress]; exists {
		t.Fatal("expired tracker hint remained in the registry")
	}
	if _, exists := client.discoveredAddresses[localAddress]; !exists {
		t.Fatal("room-lifetime local hint expired")
	}
	if _, added, _ := client.rememberDiscoveryHint(discovery.Hint{
		Address:   netip.MustParseAddrPort("203.0.113.13:9000"),
		Source:    discovery.SourceTracker,
		ExpiresAt: startedAt,
	}, startedAt); added {
		t.Fatal("already-expired tracker hint was accepted")
	}
}

func TestRenewedExpiredHintRestartsImmediateProbe(t *testing.T) {
	network := newFakeRoomNetwork()
	client := newDiscoveryTestClient(network)
	address := netip.MustParseAddrPort("203.0.113.14:9000")
	startedAt := time.Unix(3500, 0)
	client.addDiscoveryHintAt(discovery.Hint{
		Address:   address,
		Source:    discovery.SourceTracker,
		ExpiresAt: startedAt.Add(time.Second),
	}, startedAt)
	client.addDiscoveryHintAt(discovery.Hint{
		Address:   address,
		Source:    discovery.SourceTracker,
		ExpiresAt: startedAt.Add(time.Hour),
	}, startedAt.Add(2*time.Second))
	if got := len(network.sentAddresses()); got != 2 {
		t.Fatalf("renewed expired hint sent %d immediate probes, want 2", got)
	}
	if interval := client.discoveredAddresses[address].probeInterval; interval != helloInterval {
		t.Fatalf("renewed hint interval = %v, want %v", interval, helloInterval)
	}
}

func TestTrackerHintCannotEvictRoomLifetimeDiscoveryHint(t *testing.T) {
	client := newDiscoveryTestClient(newFakeRoomNetwork())
	now := time.Unix(3600, 0)
	for index := range maxDiscoveryHints {
		address := discoveryBudgetAddress(index)
		client.rememberDiscoveryHint(discovery.Hint{Address: address, Source: discovery.SourceMDNS}, now.Add(time.Duration(index)*time.Second))
	}
	trackerAddress := netip.MustParseAddrPort("198.51.100.20:9000")
	if _, added, _ := client.rememberDiscoveryHint(discovery.Hint{
		Address:   trackerAddress,
		Source:    discovery.SourceTracker,
		ExpiresAt: now.Add(time.Hour),
	}, now.Add(2*time.Hour)); added {
		t.Fatal("untrusted tracker hint displaced room-lifetime discovery")
	}
	if len(client.discoveredAddresses) != maxDiscoveryHints {
		t.Fatalf("discovery registry size = %d", len(client.discoveredAddresses))
	}
}

func TestDiscoveryHintsIgnoreInvalidAndSelfAddresses(t *testing.T) {
	network := newFakeRoomNetwork()
	network.snapshot.Endpoint.Candidates = []endpoint.Candidate{{Address: "198.51.100.20:9000"}}
	client := newDiscoveryTestClient(network)
	client.networkSnapshot.Endpoint.ListenAddress = "[::ffff:192.0.2.20]:9000"
	now := time.Unix(4000, 0)
	ignored := []discovery.Hint{
		{Address: netip.AddrPort{}, Source: discovery.SourceMDNS},
		{Address: netip.MustParseAddrPort("0.0.0.0:9000"), Source: discovery.SourceMDNS},
		{Address: netip.MustParseAddrPort("224.0.0.1:9000"), Source: discovery.SourceMDNS},
		{Address: netip.MustParseAddrPort("[fe80::1]:9000"), Source: discovery.SourceMDNS},
		{Address: netip.MustParseAddrPort("203.0.113.20:0"), Source: discovery.SourceMDNS},
		{Address: netip.MustParseAddrPort("192.0.2.20:9000"), Source: discovery.SourceTracker, ExpiresAt: now.Add(time.Minute)},
		{Address: netip.MustParseAddrPort("198.51.100.20:9000"), Source: discovery.SourceTracker, ExpiresAt: now.Add(time.Minute)},
		{Address: netip.MustParseAddrPort("203.0.113.21:9000"), Source: discovery.Source("unknown")},
		{Address: netip.MustParseAddrPort("203.0.113.22:9000"), Source: discovery.SourceTracker},
	}
	for _, hint := range ignored {
		client.addDiscoveryHintAt(hint, now)
	}
	if len(client.discoveredAddresses) != 0 || len(network.sentAddresses()) != 0 {
		t.Fatalf("ignored hints populated registry or sent Hello: records=%v sends=%v", client.discoveredAddresses, network.sentAddresses())
	}

	remote := netip.MustParseAddrPort("203.0.113.23:9000")
	client.addDiscoveryHintAt(discovery.Hint{Address: remote, Source: discovery.SourceMDNS}, now)
	if len(client.discoveredAddresses) != 1 || len(network.sentAddresses()) != 1 {
		t.Fatal("valid remote hint was ignored")
	}
}

func TestDiscoveryProbesSkipSessionPaths(t *testing.T) {
	network := newFakeRoomNetwork()
	client := newDiscoveryTestClient(network)
	address := netip.MustParseAddrPort("203.0.113.23:9000")
	path, err := NewPath(address)
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Unix(5000, 0)
	client.addDiscoveryHintAt(discovery.Hint{Address: address, Source: discovery.SourceMDNS}, startedAt)
	peerSess := &PeeringSession{path: path, authenticated: true}
	client.remotePeers["remote"] = &RemotePeer{session: peerSess}
	client.sendHellos(startedAt.Add(time.Hour))
	if got := len(network.sentAddresses()); got != 1 {
		t.Fatalf("active session path received %d periodic Hellos, want 0", got-1)
	}
	peerSess.authenticated = false
	client.sendHellos(startedAt.Add(time.Hour))
	if got := len(network.sentAddresses()); got != 2 {
		t.Fatal("due recovery probe was not sent after the session became inactive")
	}
}

func newDiscoveryTestClient(network *fakeRoomNetwork) *Client {
	return &Client{
		roomNetwork:         network,
		helloPacket:         []byte{1},
		discoveredAddresses: make(map[netip.AddrPort]discoveredAddress),
		remotePeers:         make(map[string]*RemotePeer),
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
		snapshot, diagnostics := client.StateSnapshot()
		if condition(snapshot, diagnostics) {
			return
		}
		select {
		case <-peerChanges:
		case <-deadline:
			snapshot, diagnostics := client.StateSnapshot()
			t.Fatalf("timed out waiting for client state: %#v %#v", snapshot, diagnostics)
		}
	}
}

package networking

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"bork/internal/networking/discovery"
	"bork/internal/networking/discovery/tracker"
	"bork/internal/networking/endpoint"
	"bork/internal/networking/portmap"
)

type fakeEndpoint struct {
	mu       sync.RWMutex
	snapshot endpoint.Snapshot
	changes  chan struct{}
	packets  chan endpoint.Datagram
	run      func(context.Context) error
}

func (e *fakeEndpoint) Run(ctx context.Context) error { return e.run(ctx) }
func (e *fakeEndpoint) Snapshot() endpoint.Snapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.snapshot
}
func (e *fakeEndpoint) SnapshotChanges() <-chan struct{}               { return e.changes }
func (e *fakeEndpoint) ControlPackets() <-chan endpoint.Datagram       { return e.packets }
func (e *fakeEndpoint) ReliablePackets() <-chan endpoint.Datagram      { return nil }
func (e *fakeEndpoint) BridgePackets() <-chan endpoint.Datagram        { return nil }
func (e *fakeEndpoint) AudioPackets() <-chan endpoint.Datagram         { return nil }
func (e *fakeEndpoint) InteractivePackets() <-chan endpoint.Datagram   { return nil }
func (e *fakeEndpoint) EnqueueControl([]byte, netip.AddrPort) error    { return nil }
func (e *fakeEndpoint) EnqueueBackground([]byte, netip.AddrPort) error { return nil }
func (e *fakeEndpoint) SendRealtimeBatch(endpoint.RealtimeBatch) error {
	return nil
}
func (e *fakeEndpoint) InvalidateRealtime(uint64) {}

type fakeDiscovery struct {
	started chan struct{}
	stopped chan struct{}
	hint    *discovery.Hint
}

func (d *fakeDiscovery) Run(ctx context.Context, _ [16]byte, _ netip.AddrPort, hints chan<- discovery.Hint) error {
	if d.started != nil {
		close(d.started)
	}
	if d.hint != nil {
		select {
		case hints <- *d.hint:
		case <-ctx.Done():
			return nil
		}
	}
	<-ctx.Done()
	close(d.stopped)
	return nil
}

type scriptedDiscovery struct {
	mu        sync.Mutex
	callCount int
	failures  []error
	calls     chan int
	stopped   chan struct{}
	stopOnce  sync.Once
}

func (d *scriptedDiscovery) Run(ctx context.Context, _ [16]byte, _ netip.AddrPort, _ chan<- discovery.Hint) error {
	d.mu.Lock()
	d.callCount++
	call := d.callCount
	d.mu.Unlock()
	select {
	case d.calls <- call:
	case <-ctx.Done():
		return nil
	}
	if call <= len(d.failures) {
		return d.failures[call-1]
	}
	<-ctx.Done()
	d.stopOnce.Do(func() { close(d.stopped) })
	return nil
}

type retryRequest struct {
	delay   time.Duration
	release chan struct{}
}

type fakeRoomTracker struct {
	mu            sync.Mutex
	lastPort      uint16
	ports         chan uint16
	hint          discovery.Hint
	started       chan struct{}
	stopped       chan struct{}
	statuses      []tracker.ProviderStatus
	statusChanges chan struct{}
	onCancel      func()
	runErr        error
}

func (t *fakeRoomTracker) UpdateCandidates(candidates []tracker.AnnounceCandidate) {
	if len(candidates) == 0 {
		return
	}
	port := candidates[0].Port
	t.mu.Lock()
	defer t.mu.Unlock()
	if port == t.lastPort {
		return
	}
	t.lastPort = port
	select {
	case t.ports <- port:
	default:
	}
}

func (t *fakeRoomTracker) Snapshot() []tracker.ProviderStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]tracker.ProviderStatus{}, t.statuses...)
}
func (t *fakeRoomTracker) StatusChanges() <-chan struct{} { return t.statusChanges }

func (t *fakeRoomTracker) setStatus(status tracker.ProviderStatus) {
	t.mu.Lock()
	t.statuses = []tracker.ProviderStatus{status}
	t.mu.Unlock()
	select {
	case t.statusChanges <- struct{}{}:
	default:
	}
}

func (t *fakeRoomTracker) Run(ctx context.Context, hints chan<- discovery.Hint) error {
	close(t.started)
	if t.runErr != nil {
		close(t.stopped)
		return t.runErr
	}
	if t.hint.Address.IsValid() {
		select {
		case hints <- t.hint:
		case <-ctx.Done():
			if t.onCancel != nil {
				t.onCancel()
			}
			close(t.stopped)
			return nil
		}
	}
	<-ctx.Done()
	if t.onCancel != nil {
		t.onCancel()
	}
	close(t.stopped)
	return nil
}

func TestRoomSnapshotCloneDoesNotAliasSlices(t *testing.T) {
	snapshot := RoomSnapshot{
		Endpoint: endpoint.Snapshot{
			Candidates: []endpoint.Candidate{{Address: "192.0.2.1:9000"}},
			STUN:       []endpoint.STUNResult{{Server: "stun.example:3478"}},
		},
		Tracker: []tracker.ProviderStatus{{Provider: "tracker.example", PeerAddresses: []string{"192.0.2.2:9000"}}},
	}
	clone := snapshot.Clone()
	clone.Endpoint.Candidates[0].Address = "changed"
	clone.Endpoint.STUN[0].Server = "changed"
	clone.Tracker[0].Provider = "changed"
	clone.Tracker[0].PeerAddresses[0] = "changed"

	if snapshot.Endpoint.Candidates[0].Address != "192.0.2.1:9000" || snapshot.Endpoint.STUN[0].Server != "stun.example:3478" ||
		snapshot.Tracker[0].Provider != "tracker.example" || snapshot.Tracker[0].PeerAddresses[0] != "192.0.2.2:9000" {
		t.Fatalf("clone mutation reached room snapshot: %#v", snapshot)
	}
}

type fakeRoomPortMapper struct {
	ports    chan uint16
	states   chan portmap.State
	started  chan struct{}
	stopped  chan struct{}
	onCancel func()
}

func (m *fakeRoomPortMapper) Run(ctx context.Context, port uint16, updates chan<- portmap.State) error {
	m.ports <- port
	close(m.started)
	defer close(m.stopped)
	for {
		select {
		case state := <-m.states:
			select {
			case updates <- state:
			case <-ctx.Done():
				if m.onCancel != nil {
					m.onCancel()
				}
				return nil
			}
		case <-ctx.Done():
			if m.onCancel != nil {
				m.onCancel()
			}
			return nil
		}
	}
}

func TestRoomNetworkJoinsDiscoveryOnEndpointFailure(t *testing.T) {
	endpointFailure := errors.New("endpoint failed")
	discoveryStarted := make(chan struct{})
	endpointUDP := &fakeEndpoint{
		changes: make(chan struct{}, 1),
		packets: make(chan endpoint.Datagram),
	}
	endpointUDP.run = func(context.Context) error {
		endpointUDP.mu.Lock()
		endpointUDP.snapshot.ListenAddress = "127.0.0.1:9000"
		endpointUDP.mu.Unlock()
		endpointUDP.changes <- struct{}{}
		<-discoveryStarted
		return endpointFailure
	}
	discoveryService := &fakeDiscovery{started: discoveryStarted, stopped: make(chan struct{})}
	network := newRoomNetwork(
		[16]byte{1},
		endpointUDP,
		[]discovery.Service{discoveryService},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := network.Run(context.Background()); !errors.Is(err, endpointFailure) {
		t.Fatalf("Run() error = %v", err)
	}
	select {
	case <-discoveryService.stopped:
	default:
		t.Fatal("discovery was not joined before Run returned")
	}
	if snapshot := network.Snapshot(); snapshot.NetworkError != endpointFailure.Error() {
		t.Fatalf("network error = %q", snapshot.NetworkError)
	}
}

func TestRoomNetworkRunsTrackerUpdatesMappedPortAndJoinsIt(t *testing.T) {
	endpointUDP, endpointStopped := newRunningFakeEndpoint()
	trackerHint := discovery.Hint{
		Address:   netip.MustParseAddrPort("203.0.113.50:45000"),
		Source:    discovery.SourceTracker,
		ExpiresAt: time.Now().Add(time.Minute),
	}
	trackerWorker := &fakeRoomTracker{
		ports:         make(chan uint16, 4),
		hint:          trackerHint,
		started:       make(chan struct{}),
		stopped:       make(chan struct{}),
		statusChanges: make(chan struct{}, 1),
		onCancel: func() {
			select {
			case <-endpointStopped:
				t.Error("endpoint stopped before tracker registrations were cleaned up")
			default:
			}
		},
	}
	network := newRoomNetwork(
		[16]byte{1},
		endpointUDP,
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	network.tracker = trackerWorker
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- network.Run(ctx) }()
	waitForRoomSignal(t, trackerWorker.started, "tracker startup")
	if port := waitForTrackerPort(t, trackerWorker.ports); port != 9000 {
		t.Fatalf("initial tracker port = %d, want 9000", port)
	}
	select {
	case hint := <-network.DiscoveredPeers():
		if hint != trackerHint {
			t.Fatalf("tracker hint = %#v, want %#v", hint, trackerHint)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tracker hint")
	}
	trackerStatus := tracker.ProviderStatus{Provider: "tracker.test", PeerCount: 2, PeerAddresses: []string{}}
	trackerWorker.setStatus(trackerStatus)
	waitForRoomSnapshot(t, network, func(snapshot RoomSnapshot) bool {
		return len(snapshot.Tracker) == 1 && reflect.DeepEqual(snapshot.Tracker[0], trackerStatus)
	})

	endpointUDP.mu.Lock()
	endpointUDP.snapshot.Candidates = []endpoint.Candidate{{
		Type:    endpoint.CandidateSTUN,
		Address: "198.51.100.7:46000",
		Family:  "ipv4",
	}}
	endpointUDP.mu.Unlock()
	endpointUDP.changes <- struct{}{}
	if port := waitForTrackerPort(t, trackerWorker.ports); port != 46000 {
		t.Fatalf("mapped tracker port = %d, want 46000", port)
	}

	cancel()
	if err := waitForRoomResult(t, result); err != nil {
		t.Fatal(err)
	}
	waitForRoomSignal(t, trackerWorker.stopped, "tracker shutdown")
	waitForRoomSignal(t, endpointStopped, "endpoint shutdown")
}

func TestRoomNetworkProjectsPortMappingAndUpdatesTracker(t *testing.T) {
	endpointUDP, endpointStopped := newRunningFakeEndpoint()
	endpointUDP.mu.Lock()
	endpointUDP.snapshot.ListenAddress = "0.0.0.0:9000"
	endpointUDP.snapshot.Candidates = []endpoint.Candidate{
		{Type: endpoint.CandidateNIC, Address: "192.0.2.10:9000", Family: "ipv4"},
		{Type: endpoint.CandidateSTUN, Address: "8.8.8.8:45000", Family: "ipv4"},
	}
	endpointUDP.mu.Unlock()
	endpointUDP.run = func(ctx context.Context) error {
		endpointUDP.changes <- struct{}{}
		<-ctx.Done()
		close(endpointStopped)
		return nil
	}
	trackerWorker := &fakeRoomTracker{
		ports:   make(chan uint16, 8),
		started: make(chan struct{}),
		stopped: make(chan struct{}),
	}
	mapper := &fakeRoomPortMapper{
		ports:   make(chan uint16, 1),
		states:  make(chan portmap.State, 4),
		started: make(chan struct{}),
		stopped: make(chan struct{}),
		onCancel: func() {
			select {
			case <-endpointStopped:
				t.Error("endpoint stopped before port mapping cleanup")
			default:
			}
		},
	}
	network := newRoomNetwork([16]byte{1}, endpointUDP, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	network.tracker = trackerWorker
	network.portMapper = mapper
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- network.Run(ctx) }()
	waitForRoomSignal(t, mapper.started, "port mapper startup")
	if port := waitForTrackerPort(t, mapper.ports); port != 9000 {
		t.Fatalf("mapped internal port = %d, want 9000", port)
	}
	waitForRoomSignal(t, trackerWorker.started, "tracker startup")
	if port := waitForTrackerPort(t, trackerWorker.ports); port != 45000 {
		t.Fatalf("initial tracker port = %d, want 45000", port)
	}

	mapping := portmap.Mapping{
		ExternalAddress: netip.MustParseAddrPort("8.8.8.8:47000"),
		Provider:        "UPnP IGD test",
		ExpiresAt:       time.Now().Add(time.Hour),
	}
	mapper.states <- portmap.State{Mapping: &mapping}
	waitForPortMappingSnapshot(t, network, "8.8.8.8:47000", "")
	if port := waitForTrackerPort(t, trackerWorker.ports); port != 47000 {
		t.Fatalf("mapped tracker port = %d, want 47000", port)
	}

	mapper.states <- portmap.State{Mapping: &mapping, Error: "renewal delayed"}
	waitForPortMappingSnapshot(t, network, "8.8.8.8:47000", "renewal delayed")
	mapper.states <- portmap.State{Error: "mapping expired"}
	waitForPortMappingSnapshot(t, network, "", "mapping expired")
	if port := waitForTrackerPort(t, trackerWorker.ports); port != 45000 {
		t.Fatalf("fallback tracker port = %d, want 45000", port)
	}

	cancel()
	if err := waitForRoomResult(t, result); err != nil {
		t.Fatal(err)
	}
	waitForRoomSignal(t, mapper.stopped, "port mapper shutdown")
	waitForRoomSignal(t, trackerWorker.stopped, "tracker shutdown")
	waitForRoomSignal(t, endpointStopped, "endpoint shutdown")
}

func TestRoomNetworkForwardsTypedHintAndClearsMappingAfterShutdown(t *testing.T) {
	endpointUDP, endpointStopped := newRunningFakeEndpoint()
	endpointUDP.mu.Lock()
	endpointUDP.snapshot.ListenAddress = "0.0.0.0:9000"
	endpointUDP.snapshot.Candidates = []endpoint.Candidate{{
		Type: endpoint.CandidateNIC, Address: "192.0.2.10:9000", Family: "ipv4",
	}}
	endpointUDP.mu.Unlock()
	endpointUDP.run = func(ctx context.Context) error {
		endpointUDP.changes <- struct{}{}
		<-ctx.Done()
		close(endpointStopped)
		return nil
	}
	mapper := &fakeRoomPortMapper{
		ports:   make(chan uint16, 1),
		states:  make(chan portmap.State, 1),
		started: make(chan struct{}),
		stopped: make(chan struct{}),
	}
	wantHint := discovery.Hint{
		Address:   netip.MustParseAddrPort("203.0.113.10:9000"),
		Source:    discovery.SourceTracker,
		ExpiresAt: time.Now().Add(time.Minute),
	}
	discoveryService := &fakeDiscovery{stopped: make(chan struct{}), hint: &wantHint}
	network := newRoomNetwork([16]byte{1}, endpointUDP, []discovery.Service{discoveryService}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	network.portMapper = mapper
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- network.Run(ctx) }()
	waitForRoomSignal(t, mapper.started, "port mapper startup")
	_ = waitForTrackerPort(t, mapper.ports)
	select {
	case got := <-network.DiscoveredPeers():
		if got.Address != wantHint.Address || got.Source != wantHint.Source || !got.ExpiresAt.Equal(wantHint.ExpiresAt) {
			t.Fatalf("discovery hint = %#v, want %#v", got, wantHint)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for typed discovery hint")
	}
	mapping := portmap.Mapping{ExternalAddress: netip.MustParseAddrPort("8.8.8.8:47000"), Provider: "UPnP test", ExpiresAt: time.Now().Add(time.Hour)}
	mapper.states <- portmap.State{Mapping: &mapping}
	waitForPortMappingSnapshot(t, network, mapping.ExternalAddress.String(), "")
	cancel()
	if err := waitForRoomResult(t, result); err != nil {
		t.Fatal(err)
	}
	waitForRoomSignal(t, mapper.stopped, "port mapper shutdown")
	waitForRoomSignal(t, discoveryService.stopped, "discovery shutdown")
	waitForRoomSignal(t, endpointStopped, "endpoint shutdown")
	for _, candidate := range network.Snapshot().Endpoint.Candidates {
		if candidate.Type == endpoint.CandidatePortMapped {
			t.Fatalf("final snapshot retained deleted mapping: %#v", candidate)
		}
	}
}

func TestRoomNetworkExpiresMappingWhileMapperRenewalIsBlocked(t *testing.T) {
	endpointUDP, endpointStopped := newRunningFakeEndpoint()
	endpointUDP.mu.Lock()
	endpointUDP.snapshot.ListenAddress = "0.0.0.0:9000"
	endpointUDP.snapshot.Candidates = []endpoint.Candidate{
		{Type: endpoint.CandidateNIC, Address: "192.0.2.10:9000", Family: "ipv4"},
		{Type: endpoint.CandidateSTUN, Address: "8.8.8.8:45000", Family: "ipv4"},
	}
	endpointUDP.mu.Unlock()
	endpointUDP.run = func(ctx context.Context) error {
		endpointUDP.changes <- struct{}{}
		<-ctx.Done()
		close(endpointStopped)
		return nil
	}
	trackerWorker := &fakeRoomTracker{ports: make(chan uint16, 8), started: make(chan struct{}), stopped: make(chan struct{})}
	mapper := &fakeRoomPortMapper{
		ports: make(chan uint16, 1), states: make(chan portmap.State, 1),
		started: make(chan struct{}), stopped: make(chan struct{}),
	}
	network := newRoomNetwork([16]byte{1}, endpointUDP, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	network.tracker = trackerWorker
	network.portMapper = mapper
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- network.Run(ctx) }()
	waitForRoomSignal(t, mapper.started, "port mapper startup")
	_ = waitForTrackerPort(t, mapper.ports)
	waitForRoomSignal(t, trackerWorker.started, "tracker startup")
	if port := waitForTrackerPort(t, trackerWorker.ports); port != 45000 {
		t.Fatalf("initial tracker port = %d, want 45000", port)
	}

	mapping := portmap.Mapping{
		ExternalAddress: netip.MustParseAddrPort("8.8.8.8:47000"),
		Provider:        "blocked mapper",
		ExpiresAt:       time.Now().Add(150 * time.Millisecond),
	}
	mapper.states <- portmap.State{Mapping: &mapping, Error: "renewal blocked"}
	waitForPortMappingSnapshot(t, network, mapping.ExternalAddress.String(), "renewal blocked")
	if port := waitForTrackerPort(t, trackerWorker.ports); port != 47000 {
		t.Fatalf("mapped tracker port = %d, want 47000", port)
	}
	waitForPortMappingSnapshot(t, network, "", "port mapping expired")
	if port := waitForTrackerPort(t, trackerWorker.ports); port != 45000 {
		t.Fatalf("expired mapping fallback port = %d, want 45000", port)
	}
	select {
	case <-mapper.stopped:
		t.Fatal("mapper stopped instead of remaining blocked during lease expiry")
	default:
	}

	cancel()
	if err := waitForRoomResult(t, result); err != nil {
		t.Fatal(err)
	}
	waitForRoomSignal(t, mapper.stopped, "port mapper shutdown")
	waitForRoomSignal(t, trackerWorker.stopped, "tracker shutdown")
	waitForRoomSignal(t, endpointStopped, "endpoint shutdown")
}

func TestRoomNetworkRejectsAlreadyExpiredMappingState(t *testing.T) {
	endpointUDP, endpointStopped := newRunningFakeEndpoint()
	endpointUDP.mu.Lock()
	endpointUDP.snapshot.ListenAddress = "0.0.0.0:9000"
	endpointUDP.snapshot.Candidates = []endpoint.Candidate{{Type: endpoint.CandidateNIC, Address: "192.0.2.10:9000", Family: "ipv4"}}
	endpointUDP.mu.Unlock()
	endpointUDP.run = func(ctx context.Context) error {
		endpointUDP.changes <- struct{}{}
		<-ctx.Done()
		close(endpointStopped)
		return nil
	}
	mapper := &fakeRoomPortMapper{
		ports: make(chan uint16, 1), states: make(chan portmap.State, 1),
		started: make(chan struct{}), stopped: make(chan struct{}),
	}
	network := newRoomNetwork([16]byte{1}, endpointUDP, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	network.portMapper = mapper
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- network.Run(ctx) }()
	waitForRoomSignal(t, mapper.started, "port mapper startup")
	_ = waitForTrackerPort(t, mapper.ports)
	expired := portmap.Mapping{
		ExternalAddress: netip.MustParseAddrPort("8.8.8.8:47000"), Provider: "stale mapper", ExpiresAt: time.Now().Add(-time.Second),
	}
	mapper.states <- portmap.State{Mapping: &expired}
	waitForPortMappingSnapshot(t, network, "", "port mapping expired")

	cancel()
	if err := waitForRoomResult(t, result); err != nil {
		t.Fatal(err)
	}
	waitForRoomSignal(t, mapper.stopped, "port mapper shutdown")
	waitForRoomSignal(t, endpointStopped, "endpoint shutdown")
}

func TestWithPortMappingPreservesRawEndpointSnapshot(t *testing.T) {
	raw := endpoint.Snapshot{Candidates: []endpoint.Candidate{
		{Type: endpoint.CandidateNIC, Address: "192.168.1.10:9000", Family: "ipv4"},
		{Type: endpoint.CandidateSTUN, Address: "8.8.8.8:45000", Family: "ipv4"},
	}}
	withoutMapping := withPortMapping(raw, nil)
	if !slices.Equal(withoutMapping.Candidates, raw.Candidates) || &withoutMapping.Candidates[0] != &raw.Candidates[0] {
		t.Fatal("nil mapping rewrote the endpoint snapshot")
	}
	mapping := &portmap.Mapping{ExternalAddress: netip.MustParseAddrPort("8.8.8.8:47000"), Provider: "test"}
	projected := withPortMapping(raw, mapping)
	if len(projected.Candidates) != len(raw.Candidates)+1 || projected.Candidates[len(raw.Candidates)].Type != endpoint.CandidatePortMapped {
		t.Fatalf("projected candidates = %+v", projected.Candidates)
	}
	if len(raw.Candidates) != 2 || raw.Candidates[0].Type != endpoint.CandidateNIC || raw.Candidates[1].Type != endpoint.CandidateSTUN {
		t.Fatalf("raw endpoint candidates were mutated: %+v", raw.Candidates)
	}
}

func TestTrackerAnnounceCandidatesPreserveActualEndpointPairs(t *testing.T) {
	snapshot := endpoint.Snapshot{
		ListenAddress: "0.0.0.0:9000",
		Candidates: []endpoint.Candidate{
			{Type: endpoint.CandidateSTUN, Address: "8.8.8.8:45000", Family: "ipv4", Source: "slow"},
			{Type: endpoint.CandidateSTUN, Address: "1.1.1.1:46000", Family: "ipv4", Source: "fast"},
			{Type: endpoint.CandidateSTUN, Address: "9.9.9.9:46500", Family: "ipv4", Source: "medium"},
			{Type: endpoint.CandidateSTUN, Address: "100.64.0.1:48000", Family: "ipv4"},
			{Type: endpoint.CandidatePortMapped, Address: "8.8.8.8:47000", Family: "ipv4"},
		},
		STUN: []endpoint.STUNResult{{Server: "slow", RTTMillis: 30}, {Server: "fast", RTTMillis: 10}, {Server: "medium", RTTMillis: 20}},
	}
	want := []tracker.AnnounceCandidate{
		{Address: netip.MustParseAddr("8.8.8.8"), Port: 47000},
		{Address: netip.MustParseAddr("1.1.1.1"), Port: 46000},
		{Address: netip.MustParseAddr("9.9.9.9"), Port: 46500},
		{Address: netip.MustParseAddr("8.8.8.8"), Port: 45000},
	}
	if got := trackerAnnounceCandidates(snapshot); !slices.Equal(got, want) {
		t.Fatalf("trackerAnnounceCandidates() = %+v, want %+v", got, want)
	}
}

func TestTrackerAnnounceCandidatesUseObservedFallbackOnlyWithoutPublicEndpoint(t *testing.T) {
	snapshot := endpoint.Snapshot{
		ListenAddress: "[::]:9000",
		Candidates:    []endpoint.Candidate{{Type: endpoint.CandidateNIC, Address: "192.168.1.10:9000", Family: "ipv4"}},
	}
	want := []tracker.AnnounceCandidate{{Port: 9000}}
	if got := trackerAnnounceCandidates(snapshot); !slices.Equal(got, want) {
		t.Fatalf("trackerAnnounceCandidates() = %+v, want %+v", got, want)
	}
	snapshot.Candidates = append(snapshot.Candidates, endpoint.Candidate{
		Type: endpoint.CandidateSTUN, Address: "8.8.8.8:45000", Family: "ipv4",
	})
	want = []tracker.AnnounceCandidate{{Address: netip.MustParseAddr("8.8.8.8"), Port: 45000}}
	if got := trackerAnnounceCandidates(snapshot); !slices.Equal(got, want) {
		t.Fatalf("public tracker candidates = %+v, want %+v", got, want)
	}
}

func TestPortMappingRequiresIPv4HostCandidate(t *testing.T) {
	if port := portMappingInternalPort(endpoint.Snapshot{ListenAddress: "127.0.0.1:9000"}); port != 0 {
		t.Fatalf("loopback-only mapping port = %d", port)
	}
	snapshot := endpoint.Snapshot{
		ListenAddress: "[::]:9000",
		Candidates: []endpoint.Candidate{{
			Type: endpoint.CandidateNIC, Address: "192.0.2.10:9000", Family: "ipv4",
		}},
	}
	if port := portMappingInternalPort(snapshot); port != 9000 {
		t.Fatalf("mapping port = %d, want 9000", port)
	}
	explicit := snapshot
	explicit.ListenAddress = "192.0.2.10:9000"
	if port := portMappingInternalPort(explicit); port != 0 {
		t.Fatalf("explicit-bind mapping port = %d", port)
	}
}

func TestRoomNetworkAnnouncesToHTTPTracker(t *testing.T) {
	requests := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case requests <- request.Clone(context.Background()):
		default:
		}
		_, _ = writer.Write([]byte("d8:intervali60e5:peers0:e"))
	}))
	defer server.Close()
	trackerHash := [20]byte{1, 2, 3}
	network := NewRoomNetwork([16]byte{1}, trackerHash, [32]byte{1}, Options{
		Endpoint:    endpoint.Options{ListenAddress: "127.0.0.1:0", STUNServers: []string{}, STUNRefresh: 0},
		TrackerURLs: []string{server.URL + "/announce"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- network.Run(ctx) }()
	select {
	case request := <-requests:
		query := request.URL.Query()
		if query.Get("info_hash") != string(trackerHash[:]) {
			t.Fatalf("tracker info_hash = %x", []byte(query.Get("info_hash")))
		}
		if query.Get("port") == "" || query.Get("port") == "0" || query.Get("compact") != "1" {
			t.Fatalf("tracker query = %v", query)
		}
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("room network did not announce to HTTP tracker")
	}
	cancel()
	if err := waitForRoomResult(t, result); err != nil {
		t.Fatal(err)
	}
}

func TestRoomNetworkRetriesDiscoveryWithBoundedBackoffAndClearsError(t *testing.T) {
	failures := []error{
		errors.New("discovery failure 1"),
		errors.New("discovery failure 2"),
		errors.New("discovery failure 3"),
	}
	endpointUDP, endpointStopped := newRunningFakeEndpoint()
	discoveryService := &scriptedDiscovery{
		failures: failures,
		calls:    make(chan int, 8),
		stopped:  make(chan struct{}),
	}
	network := newRoomNetwork(
		[16]byte{1},
		endpointUDP,
		[]discovery.Service{discoveryService},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	retries := make(chan retryRequest)
	network.discoveryInitial = time.Second
	network.discoveryMax = 2 * time.Second
	network.discoveryRetry = controlledDiscoveryRetry(retries)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- network.Run(ctx) }()
	for index, wantDelay := range []time.Duration{time.Second, 2 * time.Second, 2 * time.Second} {
		if call := waitForDiscoveryCall(t, discoveryService.calls); call != index+1 {
			t.Fatalf("discovery call = %d, want %d", call, index+1)
		}
		request := waitForRetryRequest(t, retries)
		if request.delay != wantDelay {
			t.Fatalf("retry delay = %v, want %v", request.delay, wantDelay)
		}
		waitForDiscoveryError(t, network, failures[index].Error())
		close(request.release)
	}
	if call := waitForDiscoveryCall(t, discoveryService.calls); call != 4 {
		t.Fatalf("discovery call = %d, want 4", call)
	}
	waitForDiscoveryError(t, network, "")
	select {
	case err := <-result:
		t.Fatalf("RoomNetwork stopped while endpoint was alive: %v", err)
	default:
	}

	cancel()
	if err := waitForRoomResult(t, result); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	waitForRoomSignal(t, discoveryService.stopped, "discovery shutdown")
	waitForRoomSignal(t, endpointStopped, "endpoint shutdown")
}

func TestRoomNetworkCancelsDiscoveryBackoff(t *testing.T) {
	failure := errors.New("discovery failed")
	endpointUDP, endpointStopped := newRunningFakeEndpoint()
	discoveryService := &scriptedDiscovery{
		failures: []error{failure},
		calls:    make(chan int, 2),
		stopped:  make(chan struct{}),
	}
	network := newRoomNetwork(
		[16]byte{1},
		endpointUDP,
		[]discovery.Service{discoveryService},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	retries := make(chan retryRequest)
	network.discoveryRetry = controlledDiscoveryRetry(retries)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- network.Run(ctx) }()
	if call := waitForDiscoveryCall(t, discoveryService.calls); call != 1 {
		t.Fatalf("discovery call = %d, want 1", call)
	}
	_ = waitForRetryRequest(t, retries)
	waitForDiscoveryError(t, network, failure.Error())
	cancel()
	if err := waitForRoomResult(t, result); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	select {
	case call := <-discoveryService.calls:
		t.Fatalf("discovery retried after cancellation (call %d)", call)
	default:
	}
	waitForRoomSignal(t, endpointStopped, "endpoint shutdown")
}

func TestRoomNetworkComposesTrackerAndLocalDiscoveryFailures(t *testing.T) {
	localFailure := errors.New("local discovery failed")
	trackerFailure := errors.New("tracker failed")
	endpointUDP, endpointStopped := newRunningFakeEndpoint()
	discoveryService := &scriptedDiscovery{
		failures: []error{localFailure}, calls: make(chan int, 2), stopped: make(chan struct{}),
	}
	trackerWorker := &fakeRoomTracker{
		ports: make(chan uint16, 2), started: make(chan struct{}), stopped: make(chan struct{}),
		statusChanges: make(chan struct{}, 1), runErr: trackerFailure,
	}
	network := newRoomNetwork(
		[16]byte{1}, endpointUDP, []discovery.Service{discoveryService}, slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	network.tracker = trackerWorker
	retries := make(chan retryRequest)
	network.discoveryRetry = controlledDiscoveryRetry(retries)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- network.Run(ctx) }()
	if call := waitForDiscoveryCall(t, discoveryService.calls); call != 1 {
		t.Fatalf("discovery call = %d, want 1", call)
	}
	_ = waitForRetryRequest(t, retries)
	waitForRoomSignal(t, trackerWorker.stopped, "tracker failure")
	waitForRoomSnapshot(t, network, func(snapshot RoomSnapshot) bool {
		return strings.Contains(snapshot.DiscoveryError, localFailure.Error()) && strings.Contains(snapshot.DiscoveryError, trackerFailure.Error())
	})

	cancel()
	if err := waitForRoomResult(t, result); err != nil {
		t.Fatal(err)
	}
	waitForRoomSignal(t, endpointStopped, "endpoint shutdown")
}

func TestDiscoveryErrorTextRecomputesAndStaysBounded(t *testing.T) {
	serviceErrors := []error{errors.New("first"), errors.New("second")}
	if got := discoveryErrorText(serviceErrors); got != "first; second" {
		t.Fatalf("discoveryErrorText() = %q", got)
	}
	serviceErrors[0] = nil
	if got := discoveryErrorText(serviceErrors); got != "second" {
		t.Fatalf("discoveryErrorText() after recovery = %q", got)
	}
	serviceErrors[1] = errors.New(strings.Repeat("x", maxDiscoveryErrorLength+100))
	got := discoveryErrorText(serviceErrors)
	if len(got) != maxDiscoveryErrorLength || !strings.HasSuffix(got, "...") {
		t.Fatalf("bounded discovery error has length %d and suffix %q", len(got), got[len(got)-3:])
	}
}

func newRunningFakeEndpoint() (*fakeEndpoint, chan struct{}) {
	stopped := make(chan struct{})
	endpointUDP := &fakeEndpoint{
		changes: make(chan struct{}, 1),
		packets: make(chan endpoint.Datagram),
	}
	endpointUDP.run = func(ctx context.Context) error {
		endpointUDP.mu.Lock()
		endpointUDP.snapshot.ListenAddress = "127.0.0.1:9000"
		endpointUDP.mu.Unlock()
		endpointUDP.changes <- struct{}{}
		<-ctx.Done()
		close(stopped)
		return nil
	}
	return endpointUDP, stopped
}

func controlledDiscoveryRetry(requests chan<- retryRequest) func(context.Context, time.Duration) bool {
	return func(ctx context.Context, delay time.Duration) bool {
		request := retryRequest{delay: delay, release: make(chan struct{})}
		select {
		case requests <- request:
		case <-ctx.Done():
			return false
		}
		select {
		case <-request.release:
			return true
		case <-ctx.Done():
			return false
		}
	}
}

func waitForDiscoveryCall(t *testing.T, calls <-chan int) int {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for discovery call")
		return 0
	}
}

func waitForRetryRequest(t *testing.T, requests <-chan retryRequest) retryRequest {
	t.Helper()
	select {
	case request := <-requests:
		return request
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for discovery retry")
		return retryRequest{}
	}
}

func waitForDiscoveryError(t *testing.T, network *RoomNetwork, want string) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		if got := network.Snapshot().DiscoveryError; got == want {
			return
		}
		select {
		case <-network.StateChanges():
		case <-timer.C:
			t.Fatalf("discovery error = %q, want %q", network.Snapshot().DiscoveryError, want)
		}
	}
}

func waitForRoomResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for RoomNetwork to stop")
		return nil
	}
}

func waitForRoomSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func waitForRoomSnapshot(t *testing.T, network *RoomNetwork, predicate func(RoomSnapshot) bool) RoomSnapshot {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		snapshot := network.Snapshot()
		if predicate(snapshot) {
			return snapshot
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("timed out waiting for room snapshot")
		}
	}
}

func waitForTrackerPort(t *testing.T, ports <-chan uint16) uint16 {
	t.Helper()
	select {
	case port := <-ports:
		return port
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tracker port")
		return 0
	}
}

func waitForPortMappingSnapshot(t *testing.T, network *RoomNetwork, address, mappingError string) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		snapshot := network.Snapshot()
		mappedAddress := ""
		for _, candidate := range snapshot.Endpoint.Candidates {
			if candidate.Type == endpoint.CandidatePortMapped {
				mappedAddress = candidate.Address
			}
		}
		if mappedAddress == address && snapshot.PortMappingError == mappingError {
			return
		}
		select {
		case <-network.StateChanges():
		case <-deadline.C:
			t.Fatalf("port mapping snapshot = address %q, error %q; want %q, %q", mappedAddress, snapshot.PortMappingError, address, mappingError)
		}
	}
}

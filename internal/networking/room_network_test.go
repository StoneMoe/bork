package networking

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"bork/internal/networking/discovery"
	"bork/internal/networking/endpoint"
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
func (e *fakeEndpoint) SnapshotChanges() <-chan struct{}         { return e.changes }
func (e *fakeEndpoint) ControlPackets() <-chan endpoint.Datagram { return e.packets }
func (e *fakeEndpoint) VoicePackets() <-chan endpoint.Datagram   { return nil }
func (e *fakeEndpoint) Send([]byte, netip.AddrPort) error        { return nil }
func (e *fakeEndpoint) SendVoiceBatch(endpoint.VoiceBatch) error { return nil }
func (e *fakeEndpoint) InvalidateVoice(uint64)                   {}

type fakeDiscovery struct {
	started chan struct{}
	stopped chan struct{}
}

func (d *fakeDiscovery) Run(ctx context.Context, _ [16]byte, _ netip.AddrPort, _ chan<- netip.AddrPort) error {
	if d.started != nil {
		close(d.started)
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

func (d *scriptedDiscovery) Run(ctx context.Context, _ [16]byte, _ netip.AddrPort, _ chan<- netip.AddrPort) error {
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

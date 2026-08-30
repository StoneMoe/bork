package intercept

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

var errUnexpectedDial = errors.New("unexpected dial")

func TestRelay_TCP_rejects_selected_flow_when_generation_is_stale(t *testing.T) {
	dialer := &fakeDialer{}
	relay := newTestRelay(t, fakeRules{selected: true}, dialer)
	relay.SetState(GenerationState{Generation: 4, Ready: true})
	flow := newRejectedTCP(testMetadata(3, 11, 443))

	err := relay.TCP(context.Background(), flow)

	if !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("TCP() error = %v, want ErrStaleGeneration", err)
	}
	if resetErr := flow.resetError(); !errors.Is(resetErr, ErrStaleGeneration) {
		t.Fatalf("reset error = %v, want ErrStaleGeneration", resetErr)
	}
	if got := dialer.tcpCallCount(); got != 0 {
		t.Fatalf("DialTCP calls = %d, want 0", got)
	}
}

func TestRelay_TCP_rejects_selected_flow_when_generation_is_not_ready(t *testing.T) {
	dialer := &fakeDialer{}
	relay := newTestRelay(t, fakeRules{selected: true}, dialer)
	relay.SetState(GenerationState{Generation: 8})
	flow := newRejectedTCP(testMetadata(8, 12, 443))

	err := relay.TCP(context.Background(), flow)

	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("TCP() error = %v, want ErrNotReady", err)
	}
	if resetErr := flow.resetError(); !errors.Is(resetErr, ErrNotReady) {
		t.Fatalf("reset error = %v, want ErrNotReady", resetErr)
	}
	if got := dialer.tcpCallCount(); got != 0 {
		t.Fatalf("DialTCP calls = %d, want 0", got)
	}
}

func TestRelay_TCP_rejects_unselected_flow_without_dialing(t *testing.T) {
	dialer := &fakeDialer{}
	relay := newTestRelay(t, fakeRules{selected: false}, dialer)
	relay.SetState(GenerationState{Generation: 2, Ready: true})
	metadata := testMetadata(2, 13, 443)
	metadata.ExecutablePath = "C:/tools/helper.exe"
	flow := newRejectedTCP(metadata)

	err := relay.TCP(context.Background(), flow)

	if !errors.Is(err, ErrUnselected) {
		t.Fatalf("TCP() error = %v, want ErrUnselected", err)
	}
	if resetErr := flow.resetError(); !errors.Is(resetErr, ErrUnselected) {
		t.Fatalf("reset error = %v, want ErrUnselected", resetErr)
	}
	if got := dialer.tcpCallCount(); got != 0 {
		t.Fatalf("DialTCP calls = %d, want 0", got)
	}
}

type fakeRules struct {
	selected bool
	err      error
}

func (rules fakeRules) Match(string) (bool, error) { return rules.selected, rules.err }

type fakeDialer struct {
	mu         sync.Mutex
	tcpCalls   []netip.AddrPort
	tcpCalled  chan struct{}
	tcpOnce    sync.Once
	udpCalls   int
	udpCalled  chan struct{}
	udpOnce    sync.Once
	tcpConn    net.Conn
	tcpErr     error
	packetConn net.PacketConn
	udpErr     error
}

func (dialer *fakeDialer) DialTCP(_ context.Context, remote netip.AddrPort) (net.Conn, error) {
	dialer.mu.Lock()
	defer dialer.mu.Unlock()
	dialer.tcpCalls = append(dialer.tcpCalls, remote)
	if dialer.tcpCalled != nil {
		dialer.tcpOnce.Do(func() { close(dialer.tcpCalled) })
	}
	if dialer.tcpErr != nil {
		return nil, dialer.tcpErr
	}
	if dialer.tcpConn == nil {
		return nil, errUnexpectedDial
	}
	return dialer.tcpConn, nil
}

func (dialer *fakeDialer) OpenUDP() (net.PacketConn, error) {
	dialer.mu.Lock()
	defer dialer.mu.Unlock()
	dialer.udpCalls++
	if dialer.udpCalled != nil {
		dialer.udpOnce.Do(func() { close(dialer.udpCalled) })
	}
	if dialer.udpErr != nil {
		return nil, dialer.udpErr
	}
	if dialer.packetConn == nil {
		return nil, errUnexpectedDial
	}
	return dialer.packetConn, nil
}

func (dialer *fakeDialer) tcpCallCount() int {
	dialer.mu.Lock()
	defer dialer.mu.Unlock()
	return len(dialer.tcpCalls)
}

func (dialer *fakeDialer) lastTCPTarget() netip.AddrPort {
	dialer.mu.Lock()
	defer dialer.mu.Unlock()
	if len(dialer.tcpCalls) == 0 {
		return netip.AddrPort{}
	}
	return dialer.tcpCalls[len(dialer.tcpCalls)-1]
}

func (dialer *fakeDialer) udpCallCount() int {
	dialer.mu.Lock()
	defer dialer.mu.Unlock()
	return dialer.udpCalls
}

type rejectedTCP struct {
	metadata Metadata
	mu       sync.Mutex
	reset    error
}

func newRejectedTCP(metadata Metadata) *rejectedTCP { return &rejectedTCP{metadata: metadata} }

func (flow *rejectedTCP) Metadata() Metadata               { return flow.metadata }
func (flow *rejectedTCP) Read([]byte) (int, error)         { return 0, io.EOF }
func (flow *rejectedTCP) Write(buffer []byte) (int, error) { return len(buffer), nil }
func (flow *rejectedTCP) Close() error                     { return nil }
func (flow *rejectedTCP) Reset(cause error) error {
	flow.mu.Lock()
	defer flow.mu.Unlock()
	flow.reset = cause
	return nil
}
func (flow *rejectedTCP) resetError() error {
	flow.mu.Lock()
	defer flow.mu.Unlock()
	return flow.reset
}

type noopBridge struct{}

func (noopBridge) Start(context.Context, Callbacks) error { return nil }
func (noopBridge) Wait(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
func (noopBridge) Close() error { return nil }

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func newTestRelay(t *testing.T, rules ExecutableMatcher, dialer Dialer) *Relay {
	t.Helper()
	relay, err := New(Options{
		Bridge: noopBridge{}, Rules: rules, Dialer: dialer,
		DNS: netip.MustParseAddr("1.1.1.1"), QueueSize: 2,
		IdleTimeout: time.Minute, Clock: &fakeClock{now: time.Unix(100, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { relay.Close() })
	return relay
}

func testMetadata(generation Generation, nativeID NativeID, remotePort uint16) Metadata {
	return Metadata{
		Generation:     generation,
		NativeID:       nativeID,
		ProcessID:      99,
		ExecutablePath: "C:/games/game.exe",
		OriginalLocal:  netip.MustParseAddrPort("10.0.0.2:40000"),
		OriginalRemote: netip.AddrPortFrom(netip.MustParseAddr("8.8.8.8"), remotePort),
	}
}

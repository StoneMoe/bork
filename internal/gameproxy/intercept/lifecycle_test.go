package intercept

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"
)

func TestRelay_Start_exposes_current_generation_to_bridge(t *testing.T) {
	bridge := &failingBridge{}
	relay := newRelayWithBridge(t, bridge)
	want := GenerationState{Generation: 7, Ready: true}
	relay.SetState(want)

	err := relay.Start(context.Background())

	if err != nil {
		t.Fatal(err)
	}
	if bridge.generationState() != want {
		t.Fatalf("generation during Start = %+v, want %+v", bridge.generationState(), want)
	}
}

func TestRelay_Start_returns_bridge_initialization_error(t *testing.T) {
	bridgeFailure := errors.New("native initialization failed")
	bridge := &failingBridge{startFailure: bridgeFailure}
	relay := newRelayWithBridge(t, bridge)

	err := relay.Start(context.Background())

	if !errors.Is(err, bridgeFailure) {
		t.Fatalf("Start() error = %v, want bridge initialization failure", err)
	}
}

func TestRelay_Start_rejects_duplicate_start(t *testing.T) {
	bridge := &failingBridge{}
	relay := newRelayWithBridge(t, bridge)
	if err := relay.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	err := relay.Start(context.Background())

	if !errors.Is(err, ErrRelayStarted) {
		t.Fatalf("second Start() error = %v, want ErrRelayStarted", err)
	}
	if bridge.startCount() != 1 {
		t.Fatalf("bridge Start calls = %d, want 1", bridge.startCount())
	}
}

func TestRelay_Run_surfaces_fatal_bridge_error(t *testing.T) {
	bridgeFailure := errors.New("native callback loop failed")
	bridge := &failingBridge{waitFailure: bridgeFailure}
	relay := newRelayWithBridge(t, bridge)
	if err := relay.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	err := relay.Run(context.Background())

	if !errors.Is(err, ErrBridgeFatal) || !errors.Is(err, bridgeFailure) {
		t.Fatalf("Run() error = %v, want ErrBridgeFatal wrapping bridge failure", err)
	}
	if bridge.closeCount() != 1 {
		t.Fatalf("bridge Close calls = %d, want 1", bridge.closeCount())
	}
}

func TestRelay_Run_treats_nil_bridge_wait_as_fatal_stop(t *testing.T) {
	relay := newRelayWithBridge(t, &failingBridge{})
	if err := relay.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	err := relay.Run(context.Background())

	if !errors.Is(err, ErrBridgeFatal) || !errors.Is(err, ErrBridgeStopped) {
		t.Fatalf("Run() error = %v, want fatal ErrBridgeStopped", err)
	}
}

func TestRelay_Run_returns_context_cancellation_without_bridge_fatal(t *testing.T) {
	relay := newRelayWithBridge(t, &cancelBridge{})
	if err := relay.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := relay.Run(ctx)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}
	if errors.Is(err, ErrBridgeFatal) {
		t.Fatalf("Run() error = %v, do not want ErrBridgeFatal", err)
	}
}

func TestRelay_Run_closes_active_flow_when_bridge_fails(t *testing.T) {
	bridgeFailure := errors.New("native callback loop failed")
	endpoint := newFakeUDPEndpoint(testMetadata(1, 51, 9000))
	bridge := &flowBridge{endpoint: endpoint, failure: bridgeFailure}
	relay, err := New(Options{
		Bridge: bridge, Rules: fakeRules{selected: true}, Dialer: &fakeDialer{packetConn: newFakePacketConn()},
		DNS: netip.MustParseAddr("1.1.1.1"), QueueSize: 2,
		IdleTimeout: time.Minute, Clock: &fakeClock{now: time.Unix(100, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	relay.SetState(GenerationState{Generation: 1, Ready: true})
	if err := relay.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	err = relay.Run(context.Background())

	if !errors.Is(err, bridgeFailure) {
		t.Fatalf("Run() error = %v, want bridge failure", err)
	}
	waitClosed(t, endpoint.closed)
}

func TestRelay_GenerationState_returns_state_set_by_SetState(t *testing.T) {
	relay := newTestRelay(t, fakeRules{selected: true}, &fakeDialer{})
	want := GenerationState{Generation: 7, Ready: true}

	relay.SetState(want)

	if got := relay.GenerationState(); got != want {
		t.Fatalf("GenerationState() = %+v, want %+v", got, want)
	}
}

func TestRelay_TCP_rejects_metadata_stamped_from_old_generation_snapshot(t *testing.T) {
	dialer := &fakeDialer{}
	relay := newTestRelay(t, fakeRules{selected: true}, dialer)
	relay.SetState(GenerationState{Generation: 8, Ready: true})
	snapshot := relay.GenerationState()
	relay.SetState(GenerationState{Generation: 9, Ready: true})
	flow := newRejectedTCP(testMetadata(snapshot.Generation, 52, 443))

	err := relay.TCP(context.Background(), flow)

	if !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("TCP() error = %v, want ErrStaleGeneration", err)
	}
	if got := dialer.tcpCallCount(); got != 0 {
		t.Fatalf("DialTCP calls = %d, want 0", got)
	}
}

type failingBridge struct {
	mu           sync.Mutex
	startFailure error
	waitFailure  error
	state        GenerationState
	starts       int
	closes       int
}

func (bridge *failingBridge) Start(_ context.Context, callbacks Callbacks) error {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	bridge.starts++
	bridge.state = callbacks.GenerationState()
	return bridge.startFailure
}
func (bridge *failingBridge) Wait(context.Context) error { return bridge.waitFailure }
func (bridge *failingBridge) Close() error {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	bridge.closes++
	return nil
}
func (bridge *failingBridge) startCount() int {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	return bridge.starts
}
func (bridge *failingBridge) generationState() GenerationState {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	return bridge.state
}
func (bridge *failingBridge) closeCount() int {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	return bridge.closes
}

type flowBridge struct {
	endpoint  NativeUDPEndpoint
	failure   error
	callbacks Callbacks
}

func (bridge *flowBridge) Start(_ context.Context, callbacks Callbacks) error {
	bridge.callbacks = callbacks
	return nil
}
func (bridge *flowBridge) Wait(ctx context.Context) error {
	if err := bridge.callbacks.UDP(ctx, bridge.endpoint); err != nil {
		return err
	}
	return bridge.failure
}
func (*flowBridge) Close() error { return nil }

type cancelBridge struct{}

func (*cancelBridge) Start(context.Context, Callbacks) error { return nil }
func (*cancelBridge) Wait(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
func (*cancelBridge) Close() error { return nil }

func newRelayWithBridge(t *testing.T, bridge Bridge) *Relay {
	t.Helper()
	relay, err := New(Options{
		Bridge: bridge, Rules: fakeRules{selected: true}, Dialer: &fakeDialer{},
		DNS: netip.MustParseAddr("1.1.1.1"), QueueSize: 2,
		IdleTimeout: time.Minute, Clock: &fakeClock{now: time.Unix(100, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return relay
}

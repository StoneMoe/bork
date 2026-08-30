package netfilter

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"runtime"
	"sync"
	"testing"

	"bork/internal/gameproxy/intercept"
)

func TestBridge_UDP_refreshes_placeholder_local_on_first_send(t *testing.T) {
	backend := &udpTestBackend{}
	callbacks := &udpTestCallbacks{
		state: intercept.GenerationState{Generation: 21, Ready: true}, accepted: make(chan udpCallbackRecord, 1),
	}
	bridge := startUDPTestBridge(t, backend, callbacks)
	created := validUDPEvent(401)
	created.Local = netip.MustParseAddrPort("0.0.0.0:0")
	bridge.udpCreated(created)
	local := netip.MustParseAddrPort("10.0.0.8:43000")
	send := validUDPSend(created.ID, netip.MustParseAddrPort("8.8.8.8:9000"), []byte("first"))
	send.Local = local

	bridge.udpSend(send)
	record := <-callbacks.accepted

	if got := record.endpoint.Metadata().OriginalLocal; got != local {
		t.Fatalf("OriginalLocal = %v, want refreshed %v", got, local)
	}
	if got := backend.suspendSnapshot(); len(got) != 0 {
		t.Fatalf("SuspendUDP IDs = %v, want none", got)
	}
}

func TestBridge_UDP_rejects_invalid_refreshed_local(t *testing.T) {
	backend := &udpTestBackend{}
	callbacks := &udpTestCallbacks{state: intercept.GenerationState{Generation: 22, Ready: true}}
	bridge := startUDPTestBridge(t, backend, callbacks)
	created := validUDPEvent(402)
	created.Local = netip.MustParseAddrPort("0.0.0.0:0")
	bridge.udpCreated(created)
	send := validUDPSend(created.ID, netip.MustParseAddrPort("8.8.8.8:9000"), []byte("first"))
	send.Local = netip.MustParseAddrPort("0.0.0.0:0")

	bridge.udpSend(send)

	if got := backend.suspendSnapshot(); !reflect.DeepEqual(got, []intercept.NativeID{created.ID}) {
		t.Fatalf("SuspendUDP IDs = %v, want [%d]", got, created.ID)
	}
	if _, calls := callbacks.counts(); calls != 0 {
		t.Fatalf("UDP callbacks = %d, want 0", calls)
	}
}

func TestBridge_UDP_generation_snapshot_runs_without_bridge_lock(t *testing.T) {
	backend := &udpTestBackend{}
	callbacks := &lockCheckingCallbacks{state: intercept.GenerationState{Generation: 23, Ready: true}}
	bridge := newTestBridge(t, backend)
	callbacks.bridge = bridge
	if err := bridge.Start(context.Background(), callbacks); err != nil {
		t.Fatal(err)
	}
	bridge.udpCreated(validUDPEvent(403))

	bridge.udpSend(validUDPSend(403, netip.MustParseAddrPort("8.8.8.8:9000"), []byte("first")))

	if !callbacks.lockWasFree {
		t.Fatal("GenerationState ran while bridge.mu was held")
	}
}

func TestBridge_UDP_concurrent_first_sends_share_one_endpoint(t *testing.T) {
	backend := &udpTestBackend{}
	callbacks := &gatedGenerationCallbacks{
		state:   intercept.GenerationState{Generation: 24, Ready: true},
		entered: make(chan struct{}), release: make(chan struct{}), accepted: make(chan intercept.NativeUDPEndpoint, 1),
	}
	bridge := startUDPTestBridge(t, backend, callbacks)
	bridge.udpCreated(validUDPEvent(404))
	firstDone := make(chan struct{})
	secondStarted := make(chan struct{})
	secondDone := make(chan struct{})
	go func() {
		bridge.udpSend(validUDPSend(404, netip.MustParseAddrPort("8.8.8.8:9000"), []byte("first")))
		close(firstDone)
	}()
	<-callbacks.entered
	go func() {
		close(secondStarted)
		bridge.udpSend(validUDPSend(404, netip.MustParseAddrPort("203.0.113.8:7777"), []byte("second")))
		close(secondDone)
	}()
	<-secondStarted
	for range 32 {
		runtime.Gosched()
	}
	select {
	case <-secondDone:
		t.Fatal("concurrent first send completed before admission resolved")
	default:
	}

	close(callbacks.release)
	<-firstDone
	<-secondDone
	endpoint := <-callbacks.accepted
	first, firstErr := endpoint.ReadDatagram(context.Background())
	second, secondErr := endpoint.ReadDatagram(context.Background())

	if firstErr != nil || secondErr != nil || string(first.Payload) != "first" || string(second.Payload) != "second" {
		t.Fatalf("shared endpoint reads = %q/%v then %q/%v", first.Payload, firstErr, second.Payload, secondErr)
	}
	if callbacks.generationCalls != 1 || callbacks.udpCalls != 1 {
		t.Fatalf("callback counts = generation %d, UDP %d, want 1 and 1", callbacks.generationCalls, callbacks.udpCalls)
	}
	if got := backend.suspendSnapshot(); len(got) != 0 {
		t.Fatalf("SuspendUDP IDs = %v, want none", got)
	}
}

func TestBridge_UDP_resets_active_endpoint_when_local_changes(t *testing.T) {
	backend := &udpTestBackend{}
	callbacks := &udpTestCallbacks{
		state: intercept.GenerationState{Generation: 25, Ready: true}, accepted: make(chan udpCallbackRecord, 1),
	}
	bridge := startUDPTestBridge(t, backend, callbacks)
	bridge.udpCreated(validUDPEvent(405))
	bridge.udpSend(validUDPSend(405, netip.MustParseAddrPort("8.8.8.8:9000"), []byte("first")))
	endpoint := (<-callbacks.accepted).endpoint
	changed := validUDPSend(405, netip.MustParseAddrPort("8.8.8.8:9000"), []byte("second"))
	changed.Local = netip.MustParseAddrPort("10.0.0.9:43001")

	bridge.udpSend(changed)
	_, readErr := endpoint.ReadDatagram(context.Background())

	if !errors.Is(readErr, intercept.ErrInvalidFlow) {
		t.Fatalf("ReadDatagram error = %v, want ErrInvalidFlow", readErr)
	}
	if got := backend.suspendSnapshot(); !reflect.DeepEqual(got, []intercept.NativeID{405}) {
		t.Fatalf("SuspendUDP IDs = %v, want [405]", got)
	}
}

func TestBridge_Close_cancels_inflight_UDP_admission_before_backend_close(t *testing.T) {
	backend := &udpTestBackend{suspendStarted: make(chan intercept.NativeID, 1)}
	callbacks := &gatedGenerationCallbacks{
		state:   intercept.GenerationState{Generation: 26, Ready: true},
		entered: make(chan struct{}), release: make(chan struct{}), accepted: make(chan intercept.NativeUDPEndpoint, 1),
	}
	bridge := startUDPTestBridge(t, backend, callbacks)
	bridge.udpCreated(validUDPEvent(406))
	sendDone := make(chan struct{})
	go func() {
		bridge.udpSend(validUDPSend(406, netip.MustParseAddrPort("8.8.8.8:9000"), []byte("first")))
		close(sendDone)
	}()
	<-callbacks.entered
	closeResult := make(chan error, 1)
	go func() { closeResult <- bridge.Close() }()

	if id := <-backend.suspendStarted; id != 406 {
		t.Fatalf("SuspendUDP ID = %d, want 406", id)
	}
	close(callbacks.release)
	<-sendDone
	if err := <-closeResult; err != nil {
		t.Fatal(err)
	}

	if callbacks.udpCalls != 0 {
		t.Fatalf("UDP callbacks = %d, want 0", callbacks.udpCalls)
	}
	if events := backend.eventSnapshot(); !reflect.DeepEqual(events, []string{"udp-suspend", "backend-close"}) {
		t.Fatalf("close events = %v, want suspend before backend close", events)
	}
}

type lockCheckingCallbacks struct {
	bridge      *Bridge
	state       intercept.GenerationState
	lockWasFree bool
}

func (*lockCheckingCallbacks) TCP(context.Context, intercept.NativeTCPFlow) error { return nil }
func (*lockCheckingCallbacks) UDP(context.Context, intercept.NativeUDPEndpoint) error {
	return nil
}
func (callbacks *lockCheckingCallbacks) GenerationState() intercept.GenerationState {
	callbacks.lockWasFree = callbacks.bridge.mu.TryLock()
	if callbacks.lockWasFree {
		callbacks.bridge.mu.Unlock()
	}
	return callbacks.state
}

type gatedGenerationCallbacks struct {
	state intercept.GenerationState

	entered  chan struct{}
	release  chan struct{}
	accepted chan intercept.NativeUDPEndpoint
	once     sync.Once

	generationCalls int
	udpCalls        int
}

func (*gatedGenerationCallbacks) TCP(context.Context, intercept.NativeTCPFlow) error { return nil }
func (callbacks *gatedGenerationCallbacks) UDP(_ context.Context, endpoint intercept.NativeUDPEndpoint) error {
	callbacks.udpCalls++
	callbacks.accepted <- endpoint
	return nil
}
func (callbacks *gatedGenerationCallbacks) GenerationState() intercept.GenerationState {
	callbacks.generationCalls++
	callbacks.once.Do(func() { close(callbacks.entered) })
	<-callbacks.release
	return callbacks.state
}

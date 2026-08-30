package netfilter

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"testing"

	"bork/internal/gameproxy/intercept"
)

var errTestUDPPost = errors.New("test UDP post failure")

type udpReceiveCall struct {
	id      intercept.NativeID
	source  netip.AddrPort
	payload []byte
}

type udpTestBackend struct {
	mu sync.Mutex

	events         []string
	receives       []udpReceiveCall
	suspendIDs     []intercept.NativeID
	postErr        error
	postHook       func(context.Context, intercept.NativeID, netip.AddrPort, []byte) error
	postStarted    chan struct{}
	postOnce       sync.Once
	suspendStarted chan intercept.NativeID
}

func (*udpTestBackend) Start(context.Context, nativeCallbackSink, []nativeRule) error { return nil }
func (*udpTestBackend) Wait(context.Context) error                                    { return nil }
func (*udpTestBackend) PostTCPReceive(context.Context, intercept.NativeID, []byte) error {
	return nil
}
func (*udpTestBackend) CloseTCP(intercept.NativeID) error { return nil }

func (backend *udpTestBackend) PostUDPReceive(
	ctx context.Context,
	id intercept.NativeID,
	source netip.AddrPort,
	payload []byte,
) error {
	backend.mu.Lock()
	backend.receives = append(backend.receives, udpReceiveCall{id: id, source: source, payload: slices.Clone(payload)})
	hook := backend.postHook
	failure := backend.postErr
	started := backend.postStarted
	backend.mu.Unlock()
	if started != nil {
		backend.postOnce.Do(func() { close(started) })
	}
	if hook != nil {
		return hook(ctx, id, source, payload)
	}
	return failure
}

func (backend *udpTestBackend) SuspendUDP(id intercept.NativeID) error {
	backend.mu.Lock()
	backend.suspendIDs = append(backend.suspendIDs, id)
	backend.events = append(backend.events, "udp-suspend")
	started := backend.suspendStarted
	backend.mu.Unlock()
	if started != nil {
		started <- id
	}
	return nil
}

func (backend *udpTestBackend) Close() error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.events = append(backend.events, "backend-close")
	return nil
}

func (backend *udpTestBackend) receiveSnapshot() []udpReceiveCall {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return slices.Clone(backend.receives)
}

func (backend *udpTestBackend) suspendSnapshot() []intercept.NativeID {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return slices.Clone(backend.suspendIDs)
}

func (backend *udpTestBackend) eventSnapshot() []string {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return slices.Clone(backend.events)
}

type udpCallbackRecord struct {
	ctx      context.Context
	endpoint intercept.NativeUDPEndpoint
}

type udpTestCallbacks struct {
	mu sync.Mutex

	state           intercept.GenerationState
	generationCalls int
	udpCalls        int
	admissionErr    error
	onUDP           func(context.Context, intercept.NativeUDPEndpoint) error
	accepted        chan udpCallbackRecord
}

func (*udpTestCallbacks) TCP(context.Context, intercept.NativeTCPFlow) error { return nil }

func (callbacks *udpTestCallbacks) UDP(ctx context.Context, endpoint intercept.NativeUDPEndpoint) error {
	callbacks.mu.Lock()
	callbacks.udpCalls++
	hook := callbacks.onUDP
	failure := callbacks.admissionErr
	accepted := callbacks.accepted
	callbacks.mu.Unlock()
	if accepted != nil {
		accepted <- udpCallbackRecord{ctx: ctx, endpoint: endpoint}
	}
	if hook != nil {
		return hook(ctx, endpoint)
	}
	return failure
}

func (callbacks *udpTestCallbacks) GenerationState() intercept.GenerationState {
	callbacks.mu.Lock()
	defer callbacks.mu.Unlock()
	callbacks.generationCalls++
	return callbacks.state
}

func (callbacks *udpTestCallbacks) setState(state intercept.GenerationState) {
	callbacks.mu.Lock()
	defer callbacks.mu.Unlock()
	callbacks.state = state
}

func (callbacks *udpTestCallbacks) counts() (int, int) {
	callbacks.mu.Lock()
	defer callbacks.mu.Unlock()
	return callbacks.generationCalls, callbacks.udpCalls
}

func startUDPTestBridge(t *testing.T, backend nativeBackend, callbacks intercept.Callbacks) *Bridge {
	t.Helper()
	bridge := newTestBridge(t, backend)
	if err := bridge.Start(context.Background(), callbacks); err != nil {
		t.Fatal(err)
	}
	return bridge
}

func validUDPEvent(id intercept.NativeID) nativeUDPCreatedEvent {
	return nativeUDPCreatedEvent{
		ID: id, PID: 51, ExecutablePath: `c:\games\game.exe`,
		Local: netip.MustParseAddrPort("10.0.0.2:42000"),
	}
}

func validUDPSend(id intercept.NativeID, remote netip.AddrPort, payload []byte) nativeUDPSendEvent {
	return nativeUDPSendEvent{
		ID: id, Local: netip.MustParseAddrPort("10.0.0.2:42000"), Remote: remote, Payload: payload,
	}
}

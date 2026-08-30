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

var errTestPost = errors.New("test post failure")

type tcpReceiveCall struct {
	id      intercept.NativeID
	payload []byte
}

type tcpTestBackend struct {
	mu sync.Mutex

	events        []string
	receives      []tcpReceiveCall
	closeIDs      []intercept.NativeID
	postFailure   error
	postFailureAt int
	postHook      func(context.Context, intercept.NativeID, []byte) error
	blockPosts    bool
	postStarted   chan struct{}
	postOnce      sync.Once
}

func (backend *tcpTestBackend) Start(context.Context, nativeCallbackSink, []nativeRule) error {
	return nil
}

func (*tcpTestBackend) Wait(context.Context) error { return nil }

func (backend *tcpTestBackend) Close() error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.events = append(backend.events, "backend-close")
	return nil
}

func (backend *tcpTestBackend) PostTCPReceive(ctx context.Context, id intercept.NativeID, payload []byte) error {
	backend.mu.Lock()
	backend.receives = append(backend.receives, tcpReceiveCall{id: id, payload: slices.Clone(payload)})
	call := len(backend.receives)
	failureAt := backend.postFailureAt
	failure := backend.postFailure
	hook := backend.postHook
	block := backend.blockPosts
	started := backend.postStarted
	backend.mu.Unlock()
	if started != nil {
		backend.postOnce.Do(func() { close(started) })
	}
	if block {
		<-ctx.Done()
		return context.Cause(ctx)
	}
	if hook != nil {
		return hook(ctx, id, payload)
	}
	if call == failureAt {
		return failure
	}
	return nil
}

func (backend *tcpTestBackend) CloseTCP(id intercept.NativeID) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.closeIDs = append(backend.closeIDs, id)
	backend.events = append(backend.events, "tcp-close")
	return nil
}

func (backend *tcpTestBackend) receiveSnapshot() []tcpReceiveCall {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return slices.Clone(backend.receives)
}

func (backend *tcpTestBackend) closeSnapshot() []intercept.NativeID {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return slices.Clone(backend.closeIDs)
}

func (backend *tcpTestBackend) eventSnapshot() []string {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return slices.Clone(backend.events)
}

func (*fakeNativeBackend) PostTCPReceive(context.Context, intercept.NativeID, []byte) error {
	return nil
}

func (*fakeNativeBackend) CloseTCP(intercept.NativeID) error { return nil }

func (*fakeNativeBackend) PostUDPReceive(context.Context, intercept.NativeID, netip.AddrPort, []byte) error {
	return nil
}

func (*fakeNativeBackend) SuspendUDP(intercept.NativeID) error { return nil }

func (*tcpTestBackend) PostUDPReceive(context.Context, intercept.NativeID, netip.AddrPort, []byte) error {
	return nil
}

func (*tcpTestBackend) SuspendUDP(intercept.NativeID) error { return nil }

type tcpCallbackRecord struct {
	ctx  context.Context
	flow intercept.NativeTCPFlow
}

type tcpTestCallbacks struct {
	mu sync.Mutex

	state           intercept.GenerationState
	generationCalls int
	tcpCalls        int
	admissionErr    error
	accepted        chan tcpCallbackRecord
}

func (callbacks *tcpTestCallbacks) TCP(ctx context.Context, flow intercept.NativeTCPFlow) error {
	callbacks.mu.Lock()
	callbacks.tcpCalls++
	err := callbacks.admissionErr
	accepted := callbacks.accepted
	callbacks.mu.Unlock()
	if accepted != nil {
		accepted <- tcpCallbackRecord{ctx: ctx, flow: flow}
	}
	return err
}

func (*tcpTestCallbacks) UDP(context.Context, intercept.NativeUDPEndpoint) error { return nil }

func (callbacks *tcpTestCallbacks) GenerationState() intercept.GenerationState {
	callbacks.mu.Lock()
	defer callbacks.mu.Unlock()
	callbacks.generationCalls++
	return callbacks.state
}

func (callbacks *tcpTestCallbacks) counts() (int, int) {
	callbacks.mu.Lock()
	defer callbacks.mu.Unlock()
	return callbacks.generationCalls, callbacks.tcpCalls
}

func startTCPTestBridge(
	t *testing.T,
	backend nativeBackend,
	callbacks intercept.Callbacks,
) *Bridge {
	t.Helper()
	bridge := newTestBridge(t, backend)
	if err := bridge.Start(context.Background(), callbacks); err != nil {
		t.Fatal(err)
	}
	return bridge
}

func validTCPEvent(id intercept.NativeID) nativeTCPConnectedEvent {
	return nativeTCPConnectedEvent{
		ID: id, PID: 41, ExecutablePath: `c:\games\game.exe`,
		Local:  netip.MustParseAddrPort("10.0.0.2:41000"),
		Remote: netip.MustParseAddrPort("8.8.8.8:443"),
	}
}

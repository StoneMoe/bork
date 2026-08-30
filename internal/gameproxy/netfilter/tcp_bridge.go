package netfilter

import (
	"context"
	"errors"
	"net/netip"
	"strings"

	"bork/internal/gameproxy/intercept"
)

func (bridge *Bridge) tcpConnected(event nativeTCPConnectedEvent) {
	callbacks, ctx, admitted := bridge.beginNativeCallback()
	if !admitted {
		bridge.recordCallbackError(bridge.backend.CloseTCP(event.ID))
		return
	}
	defer bridge.callbackWG.Done()

	state := callbacks.GenerationState()
	if !state.Ready || !validTCPConnectedEvent(event) {
		bridge.recordCallbackError(bridge.backend.CloseTCP(event.ID))
		return
	}
	metadata := intercept.Metadata{
		Generation: state.Generation, NativeID: event.ID, ProcessID: event.PID,
		ExecutablePath: strings.Clone(event.ExecutablePath),
		OriginalLocal:  event.Local, OriginalRemote: event.Remote,
	}
	flow := newTCPFlow(ctx, bridge.backend, metadata)

	bridge.mu.Lock()
	if (bridge.state != bridgeStarting && bridge.state != bridgeStarted) || bridge.callbacks == nil {
		bridge.mu.Unlock()
		bridge.recordCallbackError(flow.Close())
		return
	}
	if duplicate := bridge.flows[event.ID]; duplicate != nil {
		bridge.mu.Unlock()
		failure := &intercept.FlowError{
			NativeID: event.ID, Operation: "admit native TCP", Cause: intercept.ErrDuplicateFlow,
		}
		bridge.recordCallbackError(duplicate.Reset(failure))
		return
	}
	bridge.flows[event.ID] = flow
	bridge.mu.Unlock()

	if err := callbacks.TCP(ctx, flow); err != nil {
		bridge.recordCallbackError(flow.Reset(err))
	}
}

func (bridge *Bridge) tcpSend(id intercept.NativeID, payload []byte) {
	bridge.mu.Lock()
	flow := bridge.flows[id]
	closed := bridge.state == bridgeClosed
	bridge.mu.Unlock()
	if flow == nil {
		if !closed {
			bridge.recordCallbackError(bridge.backend.CloseTCP(id))
		}
		return
	}
	if err := flow.enqueue(payload); errors.Is(err, intercept.ErrQueueFull) {
		failure := &intercept.FlowError{
			NativeID: id, Operation: "queue native TCP send", Cause: intercept.ErrQueueFull,
		}
		bridge.recordCallbackError(flow.Reset(failure))
	}
}

func (bridge *Bridge) tcpClosed(id intercept.NativeID) {
	bridge.mu.Lock()
	flow := bridge.flows[id]
	if flow != nil {
		delete(bridge.flows, id)
	}
	bridge.mu.Unlock()
	if flow != nil {
		flow.nativeClosed()
	}
}

func (bridge *Bridge) beginNativeCallback() (intercept.Callbacks, context.Context, bool) {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if bridge.state != bridgeStarting && bridge.state != bridgeStarted {
		return nil, nil, false
	}
	bridge.callbackWG.Add(1)
	return bridge.callbacks, bridge.startCtx, true
}

func validTCPConnectedEvent(event nativeTCPConnectedEvent) bool {
	return validIPv4Endpoint(event.Local) && validIPv4Endpoint(event.Remote) &&
		event.ExecutablePath != "" && strings.IndexByte(event.ExecutablePath, 0) < 0 &&
		isAbsoluteWindowsPath(event.ExecutablePath)
}

func validIPv4Endpoint(endpoint netip.AddrPort) bool {
	return endpoint.IsValid() && endpoint.Addr().Is4() && endpoint.Port() != 0
}

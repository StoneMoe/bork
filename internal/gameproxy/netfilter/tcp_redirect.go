//go:build game_proxy

package netfilter

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"bork/internal/gameproxy/intercept"
)

const (
	tcpRedirectAcceptTimeout = 10 * time.Second
	tcpRedirectClosedGrace   = time.Second
	maxUnexpectedTCPAccepts  = 8
	maxPendingTCPRedirects   = 256
)

type tcpRedirectRequest struct {
	metadata     intercept.Metadata
	listener     *net.TCPListener
	ruleRevision uint64
	nativeClosed bool
}

type redirectedTCPFlow struct {
	connection *net.TCPConn
	metadata   intercept.Metadata
	onClose    func()

	mu       sync.Mutex
	closed   bool
	closeErr error
}

var _ intercept.NativeTCPFlow = (*redirectedTCPFlow)(nil)

func (bridge *Bridge) tcpConnectRequest(event nativeTCPConnectRequestEvent) uint16 {
	callbacks, _, admitted := bridge.beginNativeCallback()
	if !admitted {
		return 0
	}
	defer bridge.callbackWG.Done()

	state := callbacks.GenerationState()
	if !state.Ready {
		bridge.recordCallbackError(&intercept.FlowError{
			NativeID: event.ID, Operation: "redirect TCP connect", Cause: intercept.ErrNotReady,
		})
		return 0
	}
	if !validTCPConnectRequestEvent(event) {
		bridge.recordCallbackError(&intercept.FlowError{
			NativeID: event.ID, Operation: "redirect TCP connect", Cause: intercept.ErrInvalidFlow,
		})
		return 0
	}

	bridge.mu.Lock()
	_, duplicatePending := bridge.tcpPending[event.ID]
	_, duplicateActive := bridge.tcpRedirects[event.ID]
	active := bridge.state == bridgeStarting || bridge.state == bridgeStarted
	ruleRevision := bridge.ruleRevision
	bridge.mu.Unlock()
	if duplicatePending || duplicateActive {
		bridge.recordCallbackError(&intercept.FlowError{
			NativeID: event.ID, Operation: "redirect TCP connect", Cause: intercept.ErrDuplicateFlow,
		})
		return 0
	}
	if !active {
		return 0
	}
	select {
	case bridge.tcpRedirectSlots <- struct{}{}:
	default:
		bridge.recordCallbackError(&intercept.FlowError{
			NativeID: event.ID, Operation: "listen for redirected TCP",
			Cause: errors.Join(ErrTCPRedirectUnavailable, intercept.ErrQueueFull),
		})
		return 0
	}
	slotOwned := true
	defer func() {
		if slotOwned {
			<-bridge.tcpRedirectSlots
		}
	}()

	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		bridge.recordCallbackError(&intercept.FlowError{
			NativeID: event.ID, Operation: "listen for redirected TCP", Cause: errors.Join(ErrTCPRedirectUnavailable, err),
		})
		return 0
	}
	if err := listener.SetDeadline(time.Now().Add(tcpRedirectAcceptTimeout)); err != nil {
		bridge.recordCallbackError(errors.Join(
			&intercept.FlowError{NativeID: event.ID, Operation: "set redirected TCP deadline", Cause: err},
			listener.Close(),
		))
		return 0
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || address.Port < 1 || address.Port > 65535 {
		bridge.recordCallbackError(errors.Join(
			&intercept.FlowError{NativeID: event.ID, Operation: "resolve redirected TCP listener", Cause: ErrTCPRedirectUnavailable},
			listener.Close(),
		))
		return 0
	}
	request := &tcpRedirectRequest{
		metadata: intercept.Metadata{
			Generation: state.Generation, NativeID: event.ID, ProcessID: event.PID,
			ExecutablePath: strings.Clone(event.ExecutablePath), NativeRuleMatched: true,
			OriginalLocal: event.Local, OriginalRemote: event.Remote,
		},
		listener: listener, ruleRevision: ruleRevision,
	}

	bridge.mu.Lock()
	_, duplicatePending = bridge.tcpPending[event.ID]
	_, duplicateActive = bridge.tcpRedirects[event.ID]
	active = bridge.state == bridgeStarting || bridge.state == bridgeStarted
	currentRules := request.ruleRevision == bridge.ruleRevision
	if active && currentRules && !duplicatePending && !duplicateActive {
		bridge.tcpPending[event.ID] = request
		bridge.callbackWG.Add(1)
	}
	bridge.mu.Unlock()
	if !active || !currentRules || duplicatePending || duplicateActive {
		_ = listener.Close()
		if duplicatePending || duplicateActive {
			bridge.recordCallbackError(&intercept.FlowError{
				NativeID: event.ID, Operation: "redirect TCP connect", Cause: intercept.ErrDuplicateFlow,
			})
		}
		if active && !currentRules {
			bridge.recordCallbackError(&intercept.FlowError{
				NativeID: event.ID, Operation: "redirect TCP connect", Cause: intercept.ErrStaleGeneration,
			})
		}
		return 0
	}
	go bridge.acceptRedirectedTCP(request)
	slotOwned = false
	return uint16(address.Port)
}

func (bridge *Bridge) acceptRedirectedTCP(request *tcpRedirectRequest) {
	defer bridge.callbackWG.Done()
	defer request.listener.Close()
	defer func() { <-bridge.tcpRedirectSlots }()

	for rejected := 0; rejected < maxUnexpectedTCPAccepts; rejected++ {
		connection, err := request.listener.AcceptTCP()
		if err != nil {
			bridge.finishTCPRedirectRequest(request, nil, err)
			return
		}
		remote, ok := connection.RemoteAddr().(*net.TCPAddr)
		if !ok || remote.Port != int(request.metadata.OriginalLocal.Port()) {
			_ = connection.Close()
			continue
		}
		// Query the client-side transport tuple, not the pre-redirect destination.
		// Windows may select a different source address for the loopback route.
		local := connection.LocalAddr().(*net.TCPAddr)
		pid, ownerErr := bridge.lookupTCPOwner(remote.AddrPort(), local.AddrPort())
		if ownerErr != nil || pid == 0 || pid != request.metadata.ProcessID {
			_ = connection.Close()
			bridge.recordCallbackError(&intercept.FlowError{
				NativeID: request.metadata.NativeID, Operation: "authenticate redirected TCP",
				Cause: errors.Join(ErrTCPRedirectOwner, ownerErr),
			})
			continue
		}
		bridge.finishTCPRedirectRequest(request, connection, nil)
		return
	}
	bridge.finishTCPRedirectRequest(request, nil, ErrTCPRedirectMapping)
}

func (bridge *Bridge) finishTCPRedirectRequest(request *tcpRedirectRequest, connection *net.TCPConn, acceptErr error) {
	bridge.mu.Lock()
	found := bridge.tcpPending[request.metadata.NativeID] == request
	if found {
		delete(bridge.tcpPending, request.metadata.NativeID)
	}
	callbacks := bridge.callbacks
	ctx := bridge.startCtx
	active := bridge.state == bridgeStarting || bridge.state == bridgeStarted
	nativeClosed := request.nativeClosed
	if connection == nil || !found || !active || callbacks == nil {
		bridge.mu.Unlock()
		if connection != nil {
			_ = connection.Close()
		}
		if found && active && !nativeClosed {
			bridge.recordCallbackError(&intercept.FlowError{
				NativeID: request.metadata.NativeID, Operation: "accept redirected TCP", Cause: errors.Join(ErrTCPRedirectMapping, acceptErr),
			})
		}
		return
	}

	var flow *redirectedTCPFlow
	flow = &redirectedTCPFlow{
		connection: connection,
		metadata:   request.metadata,
		onClose: func() {
			bridge.mu.Lock()
			if bridge.tcpRedirects[request.metadata.NativeID] == flow {
				delete(bridge.tcpRedirects, request.metadata.NativeID)
			}
			bridge.mu.Unlock()
		},
	}
	bridge.tcpRedirects[request.metadata.NativeID] = flow
	bridge.mu.Unlock()

	go bridge.invokeTCPCallback(callbacks, ctx, flow)
}

func (bridge *Bridge) invokeTCPCallback(callbacks intercept.Callbacks, ctx context.Context, flow intercept.NativeTCPFlow) {
	defer func() {
		if value := recover(); value != nil {
			failure := &NativeCallbackError{Event: nativeEventTCPConnected, ID: flow.Metadata().NativeID, Cause: fmt.Errorf("callback panic: %v", value)}
			_ = flow.Reset(failure)
			bridge.reportFatal(failure)
		}
	}()
	if err := callbacks.TCP(ctx, flow); err != nil {
		bridge.recordCallbackError(errors.Join(err, flow.Reset(err)))
	}
}

func (bridge *Bridge) invokeUDPCallback(callbacks intercept.Callbacks, ctx context.Context, endpoint intercept.NativeUDPEndpoint) {
	defer func() {
		if value := recover(); value != nil {
			failure := &NativeCallbackError{Event: nativeEventUDPCreated, ID: endpoint.Metadata().NativeID, Cause: fmt.Errorf("callback panic: %v", value)}
			_ = endpoint.Reset(failure)
			bridge.reportFatal(failure)
		}
	}()
	if err := callbacks.UDP(ctx, endpoint); err != nil {
		bridge.recordCallbackError(errors.Join(err, endpoint.Reset(err)))
	}
}

func (bridge *Bridge) detachTCPRedirectLocked() ([]*net.TCPListener, []*redirectedTCPFlow) {
	listeners := make([]*net.TCPListener, 0, len(bridge.tcpPending))
	for id, request := range bridge.tcpPending {
		listeners = append(listeners, request.listener)
		delete(bridge.tcpPending, id)
	}
	flows := make([]*redirectedTCPFlow, 0, len(bridge.tcpRedirects))
	for id, flow := range bridge.tcpRedirects {
		flows = append(flows, flow)
		delete(bridge.tcpRedirects, id)
	}
	return listeners, flows
}

func validTCPConnectRequestEvent(event nativeTCPConnectRequestEvent) bool {
	pathValid := event.ExecutablePath == "" ||
		strings.IndexByte(event.ExecutablePath, 0) < 0 && isAbsoluteWindowsPath(event.ExecutablePath)
	return event.PID != 0 && validIPv4Endpoint(event.Local) && validIPv4Endpoint(event.Remote) && pathValid
}

func (flow *redirectedTCPFlow) Metadata() intercept.Metadata { return flow.metadata }

func (flow *redirectedTCPFlow) Read(payload []byte) (int, error) {
	return flow.connection.Read(payload)
}

func (flow *redirectedTCPFlow) Write(payload []byte) (int, error) {
	return flow.connection.Write(payload)
}

func (flow *redirectedTCPFlow) CloseRead() error { return flow.connection.CloseRead() }

func (flow *redirectedTCPFlow) CloseWrite() error { return flow.connection.CloseWrite() }

func (flow *redirectedTCPFlow) Reset(error) error {
	flow.mu.Lock()
	if flow.closed {
		err := flow.closeErr
		flow.mu.Unlock()
		return err
	}
	lingerErr := flow.connection.SetLinger(0)
	flow.mu.Unlock()
	return errors.Join(lingerErr, flow.Close())
}

func (flow *redirectedTCPFlow) Close() error {
	flow.mu.Lock()
	if flow.closed {
		err := flow.closeErr
		flow.mu.Unlock()
		return err
	}
	flow.closed = true
	flow.closeErr = flow.connection.Close()
	err := flow.closeErr
	flow.mu.Unlock()
	if flow.onClose != nil {
		flow.onClose()
	}
	return err
}

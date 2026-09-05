package netfilter

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync"

	"bork/internal/gameproxy/intercept"
)

type bridgeState uint8

const (
	bridgeNew bridgeState = iota
	bridgeStarting
	bridgeStarted
	bridgeFailed
	bridgeClosed
)

type Bridge struct {
	backend        nativeBackend
	rules          []nativeRule
	lookupTCPOwner func(netip.AddrPort, netip.AddrPort) (intercept.ProcessID, error)

	mu               sync.Mutex
	nativeMu         sync.Mutex
	state            bridgeState
	callbacks        intercept.Callbacks
	startCtx         context.Context
	ruleRevision     uint64
	flows            map[intercept.NativeID]*tcpFlow
	tcpPending       map[intercept.NativeID]*tcpRedirectRequest
	tcpRedirects     map[intercept.NativeID]*redirectedTCPFlow
	tcpRedirectSlots chan struct{}
	udpSockets       map[intercept.NativeID]nativeUDPCreatedEvent
	udpEndpoints     map[intercept.NativeID]*udpEndpoint
	udpAdmissions    map[intercept.NativeID]*udpAdmission
	callbackWG       sync.WaitGroup
	fatalSignal      chan struct{}
	fatalOnce        sync.Once
	fatalErr         error

	closeOnce       sync.Once
	nativeCloseOnce sync.Once
	closeErr        error
	nativeCloseErr  error
}

var _ intercept.Bridge = (*Bridge)(nil)

func newBridge(executablePaths []string, backend nativeBackend) (*Bridge, error) {
	if backend == nil {
		return nil, ErrNilBackend
	}
	rules, err := exactRules(executablePaths)
	if err != nil {
		return nil, err
	}
	return &Bridge{
		backend: backend, rules: rules,
		lookupTCPOwner:   tcpSocketOwner,
		flows:            make(map[intercept.NativeID]*tcpFlow),
		tcpPending:       make(map[intercept.NativeID]*tcpRedirectRequest),
		tcpRedirects:     make(map[intercept.NativeID]*redirectedTCPFlow),
		tcpRedirectSlots: make(chan struct{}, maxPendingTCPRedirects),
		udpSockets:       make(map[intercept.NativeID]nativeUDPCreatedEvent),
		udpEndpoints:     make(map[intercept.NativeID]*udpEndpoint),
		udpAdmissions:    make(map[intercept.NativeID]*udpAdmission),
		fatalSignal:      make(chan struct{}),
	}, nil
}

func (bridge *Bridge) Start(ctx context.Context, callbacks intercept.Callbacks) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if callbacks == nil {
		return ErrNilCallbacks
	}
	bridge.mu.Lock()
	switch bridge.state {
	case bridgeNew:
		bridge.state = bridgeStarting
		bridge.callbacks = callbacks
		bridge.startCtx = ctx
	case bridgeClosed:
		bridge.mu.Unlock()
		return ErrClosed
	default:
		bridge.mu.Unlock()
		return ErrAlreadyStarted
	}
	bridge.mu.Unlock()

	err := bridge.startNative(ctx)
	if err != nil {
		return errors.Join(err, bridge.failStart(), bridge.closeNative())
	}

	return nil
}

func (bridge *Bridge) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	bridge.mu.Lock()
	state := bridge.state
	bridge.mu.Unlock()
	switch state {
	case bridgeStarted:
		waitCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		nativeResult := make(chan error, 1)
		go func() { nativeResult <- bridge.backend.Wait(waitCtx) }()
		select {
		case err := <-nativeResult:
			return err
		case <-bridge.fatalSignal:
			bridge.mu.Lock()
			err := bridge.fatalErr
			bridge.mu.Unlock()
			cancel()
			<-nativeResult
			return err
		case <-ctx.Done():
			cancel()
			<-nativeResult
			return ctx.Err()
		}
	case bridgeClosed:
		return ErrClosed
	default:
		return ErrNotStarted
	}
}

func (bridge *Bridge) UpdateRules(ctx context.Context, executablePaths []string) error {
	rules, err := exactRules(executablePaths)
	if err != nil {
		return err
	}
	bridge.nativeMu.Lock()
	defer bridge.nativeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	bridge.mu.Lock()
	active := bridge.state == bridgeStarted
	bridge.mu.Unlock()
	if !active {
		return ErrNotStarted
	}
	if err := bridge.backend.UpdateRules(ctx, rules); err != nil {
		return err
	}
	bridge.mu.Lock()
	var listeners []*net.TCPListener
	var pendingUDP []intercept.NativeID
	if bridge.state == bridgeStarted {
		bridge.rules = rules
		bridge.ruleRevision++
		listeners, pendingUDP = bridge.detachPendingRuleUpdateLocked()
	} else {
		active = false
	}
	bridge.mu.Unlock()
	if !active {
		return ErrClosed
	}
	var cleanupErrs []error
	for _, listener := range listeners {
		cleanupErrs = append(cleanupErrs, listener.Close())
	}
	for _, id := range pendingUDP {
		cleanupErrs = append(cleanupErrs, bridge.backend.SuspendUDP(id))
	}
	return errors.Join(cleanupErrs...)
}

func (bridge *Bridge) detachPendingRuleUpdateLocked() ([]*net.TCPListener, []intercept.NativeID) {
	listeners := make([]*net.TCPListener, 0, len(bridge.tcpPending))
	for id, request := range bridge.tcpPending {
		listeners = append(listeners, request.listener)
		delete(bridge.tcpPending, id)
	}
	pendingSet := make(map[intercept.NativeID]struct{}, len(bridge.udpSockets)+len(bridge.udpAdmissions))
	for id := range bridge.udpSockets {
		pendingSet[id] = struct{}{}
		delete(bridge.udpSockets, id)
	}
	for id, admission := range bridge.udpAdmissions {
		pendingSet[id] = struct{}{}
		delete(bridge.udpAdmissions, id)
		close(admission.done)
	}
	pending := make([]intercept.NativeID, 0, len(pendingSet))
	for id := range pendingSet {
		pending = append(pending, id)
	}
	return listeners, pending
}

func (bridge *Bridge) Close() error {
	bridge.closeOnce.Do(func() {
		bridge.mu.Lock()
		bridge.state = bridgeClosed
		bridge.callbacks = nil
		bridge.startCtx = nil
		listeners, redirects := bridge.detachTCPRedirectLocked()
		flows, endpoints, pending := bridge.detachNativeOperationsLocked()
		bridge.mu.Unlock()
		var operationErrs []error
		for _, listener := range listeners {
			operationErrs = append(operationErrs, listener.Close())
		}
		for _, flow := range redirects {
			operationErrs = append(operationErrs, flow.Close())
		}
		for _, endpoint := range endpoints {
			operationErrs = append(operationErrs, endpoint.Close())
		}
		for _, id := range pending {
			operationErrs = append(operationErrs, bridge.backend.SuspendUDP(id))
		}
		for _, flow := range flows {
			operationErrs = append(operationErrs, flow.Close())
		}
		bridge.callbackWG.Wait()
		bridge.closeErr = errors.Join(errors.Join(operationErrs...), bridge.closeNative())
	})
	return bridge.closeErr
}

func (*Bridge) nativeCallbackSink() {}

func (bridge *Bridge) startNative(ctx context.Context) error {
	bridge.nativeMu.Lock()
	defer bridge.nativeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	bridge.mu.Lock()
	closed := bridge.state == bridgeClosed
	bridge.mu.Unlock()
	if closed {
		return ErrClosed
	}
	bridge.mu.Lock()
	rules := slices.Clone(bridge.rules)
	bridge.mu.Unlock()
	err := bridge.backend.Start(ctx, bridge, rules)
	if err != nil {
		bridge.markStartFailed()
		return err
	}
	if err := ctx.Err(); err != nil {
		bridge.markStartFailed()
		return err
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if bridge.state == bridgeClosed {
		return ErrClosed
	}
	bridge.state = bridgeStarted
	return nil
}

func (bridge *Bridge) markStartFailed() {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if bridge.state != bridgeClosed {
		bridge.state = bridgeFailed
		bridge.callbacks = nil
		bridge.startCtx = nil
	}
}

func (bridge *Bridge) failStart() error {
	bridge.mu.Lock()
	if bridge.state == bridgeClosed {
		bridge.mu.Unlock()
		return nil
	}
	bridge.state = bridgeFailed
	bridge.callbacks = nil
	bridge.startCtx = nil
	listeners, redirects := bridge.detachTCPRedirectLocked()
	flows, endpoints, pending := bridge.detachNativeOperationsLocked()
	bridge.mu.Unlock()
	var operationErrs []error
	for _, listener := range listeners {
		operationErrs = append(operationErrs, listener.Close())
	}
	for _, flow := range redirects {
		operationErrs = append(operationErrs, flow.Close())
	}
	for _, endpoint := range endpoints {
		operationErrs = append(operationErrs, endpoint.Close())
	}
	for _, id := range pending {
		operationErrs = append(operationErrs, bridge.backend.SuspendUDP(id))
	}
	for _, flow := range flows {
		operationErrs = append(operationErrs, flow.Close())
	}
	bridge.callbackWG.Wait()
	return errors.Join(operationErrs...)
}

func (bridge *Bridge) recordCallbackError(err error) {
	if err == nil {
		return
	}
	bridge.mu.Lock()
	callbacks := bridge.callbacks
	active := bridge.state == bridgeStarting || bridge.state == bridgeStarted
	bridge.mu.Unlock()
	if active && callbacks != nil {
		go bridge.invokeEndpointError(callbacks, err)
	}
}

func (bridge *Bridge) invokeEndpointError(callbacks intercept.Callbacks, err error) {
	defer func() {
		if value := recover(); value != nil {
			bridge.reportFatal(&NativeCallbackError{
				Event: nativeEvent(0), Cause: fmt.Errorf("endpoint error callback panic: %v", value),
			})
		}
	}()
	callbacks.EndpointError(err)
}

func (bridge *Bridge) reportFatal(err error) {
	if err == nil {
		return
	}
	bridge.fatalOnce.Do(func() {
		bridge.mu.Lock()
		bridge.fatalErr = err
		bridge.mu.Unlock()
		close(bridge.fatalSignal)
	})
}

func (bridge *Bridge) closeNative() error {
	bridge.nativeCloseOnce.Do(func() {
		bridge.nativeMu.Lock()
		defer bridge.nativeMu.Unlock()
		bridge.nativeCloseErr = bridge.backend.Close()
	})
	return bridge.nativeCloseErr
}

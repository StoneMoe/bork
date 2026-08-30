package netfilter

import (
	"context"
	"errors"
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
	backend nativeBackend
	rules   []nativeRule

	mu            sync.Mutex
	nativeMu      sync.Mutex
	state         bridgeState
	callbacks     intercept.Callbacks
	startCtx      context.Context
	flows         map[intercept.NativeID]*tcpFlow
	udpSockets    map[intercept.NativeID]nativeUDPCreatedEvent
	udpEndpoints  map[intercept.NativeID]*udpEndpoint
	udpAdmissions map[intercept.NativeID]*udpAdmission
	callbackWG    sync.WaitGroup
	callbackErrs  []error

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
		flows:         make(map[intercept.NativeID]*tcpFlow),
		udpSockets:    make(map[intercept.NativeID]nativeUDPCreatedEvent),
		udpEndpoints:  make(map[intercept.NativeID]*udpEndpoint),
		udpAdmissions: make(map[intercept.NativeID]*udpAdmission),
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

	bridge.mu.Lock()
	if bridge.state == bridgeClosed {
		bridge.mu.Unlock()
		return errors.Join(ErrClosed, bridge.closeNative())
	}
	bridge.state = bridgeStarted
	bridge.mu.Unlock()
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
		return bridge.backend.Wait(ctx)
	case bridgeClosed:
		return ErrClosed
	default:
		return ErrNotStarted
	}
}

func (bridge *Bridge) Close() error {
	bridge.closeOnce.Do(func() {
		bridge.mu.Lock()
		bridge.state = bridgeClosed
		bridge.callbacks = nil
		bridge.startCtx = nil
		flows, endpoints, pending := bridge.detachNativeOperationsLocked()
		bridge.mu.Unlock()
		var operationErrs []error
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
		bridge.mu.Lock()
		callbackErr := errors.Join(bridge.callbackErrs...)
		bridge.mu.Unlock()
		bridge.closeErr = errors.Join(errors.Join(operationErrs...), callbackErr, bridge.closeNative())
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
	err := bridge.backend.Start(ctx, bridge, slices.Clone(bridge.rules))
	if err != nil {
		return err
	}
	return ctx.Err()
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
	flows, endpoints, pending := bridge.detachNativeOperationsLocked()
	bridge.mu.Unlock()
	var operationErrs []error
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
	bridge.mu.Lock()
	callbackErr := errors.Join(bridge.callbackErrs...)
	bridge.mu.Unlock()
	return errors.Join(errors.Join(operationErrs...), callbackErr)
}

func (bridge *Bridge) recordCallbackError(err error) {
	if err == nil {
		return
	}
	bridge.mu.Lock()
	bridge.callbackErrs = append(bridge.callbackErrs, err)
	bridge.mu.Unlock()
}

func (bridge *Bridge) closeNative() error {
	bridge.nativeCloseOnce.Do(func() {
		bridge.nativeMu.Lock()
		defer bridge.nativeMu.Unlock()
		bridge.nativeCloseErr = bridge.backend.Close()
	})
	return bridge.nativeCloseErr
}

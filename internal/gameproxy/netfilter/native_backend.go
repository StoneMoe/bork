package netfilter

import (
	"context"
	"sync"
)

type nativeBackendState uint8

const (
	nativeBackendNew nativeBackendState = iota
	nativeBackendStarting
	nativeBackendStarted
	nativeBackendFailed
	nativeBackendClosing
	nativeBackendClosed
)

type sdkNativeBackend struct {
	config      nativeConfig
	sdk         nativeSDK
	coordinator *nativeProcessCoordinator

	mu            sync.Mutex
	state         nativeBackendState
	sink          nativeCallbackSink
	token         uint64
	ownsProcess   bool
	initSucceeded bool
	fatal         error
	operations    sync.WaitGroup
	signal        chan struct{}
	signalOnce    sync.Once
	closeOnce     sync.Once
	closeErr      error
}

var _ nativeBackend = (*sdkNativeBackend)(nil)

func newSDKNativeBackend(config nativeConfig, sdk nativeSDK, coordinator *nativeProcessCoordinator) *sdkNativeBackend {
	return &sdkNativeBackend{config: config, sdk: sdk, coordinator: coordinator, signal: make(chan struct{})}
}

func (backend *sdkNativeBackend) Start(ctx context.Context, sink nativeCallbackSink, rules []nativeRule) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	backend.mu.Lock()
	if backend.state != nativeBackendNew {
		backend.mu.Unlock()
		return ErrAlreadyStarted
	}
	backend.state = nativeBackendStarting
	backend.sink = sink
	backend.token = registerNativeOwner(backend)
	token := backend.token
	backend.mu.Unlock()

	if err := backend.coordinator.acquire(token, backend.config); err != nil {
		backend.mu.Lock()
		backend.state = nativeBackendFailed
		backend.mu.Unlock()
		return err
	}
	backend.mu.Lock()
	backend.ownsProcess = true
	backend.mu.Unlock()

	result := backend.sdk.Start(token, backend.config, rules)
	if result.initAttempted {
		backend.coordinator.pin(token, backend.config)
	}
	backend.mu.Lock()
	backend.initSucceeded = result.initSucceeded
	if result.status == nativeStatusSuccess {
		backend.state = nativeBackendStarted
	} else {
		backend.state = nativeBackendFailed
	}
	backend.mu.Unlock()
	if result.status != nativeStatusSuccess {
		operation := nativeOperationInit
		if result.initSucceeded {
			operation = nativeOperationSetRules
		}
		return &NativeStartError{Operation: operation, Status: result.status, SystemError: result.systemError}
	}
	return nil
}

func (backend *sdkNativeBackend) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-backend.signal:
		backend.mu.Lock()
		defer backend.mu.Unlock()
		if backend.fatal != nil {
			return backend.fatal
		}
		return ErrClosed
	}
}

func (backend *sdkNativeBackend) reportFatal(err error) {
	if err == nil {
		return
	}
	backend.mu.Lock()
	if backend.fatal == nil {
		backend.fatal = err
		backend.signalOnce.Do(func() { close(backend.signal) })
	}
	backend.mu.Unlock()
}

func (backend *sdkNativeBackend) Close() error {
	backend.closeOnce.Do(func() {
		backend.mu.Lock()
		backend.state = nativeBackendClosing
		token := backend.token
		ownsProcess := backend.ownsProcess
		initSucceeded := backend.initSucceeded
		backend.mu.Unlock()
		if ownsProcess {
			backend.sdk.DisableCallbacks(token)
			backend.operations.Wait()
			backend.sdk.DrainCallbacks(token)
			backend.sdk.Shutdown(token, initSucceeded)
			backend.coordinator.release(token)
		}
		if token != 0 {
			unregisterNativeOwner(token)
		}
		backend.mu.Lock()
		backend.sink = nil
		backend.state = nativeBackendClosed
		backend.signalOnce.Do(func() { close(backend.signal) })
		backend.mu.Unlock()
	})
	return backend.closeErr
}

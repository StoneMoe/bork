package netfilter

import (
	"context"
	"fmt"
	"net/netip"

	"bork/internal/gameproxy/intercept"
)

func (backend *sdkNativeBackend) PostTCPReceive(ctx context.Context, id intercept.NativeID, payload []byte) error {
	if err := backend.beginOperation(ctx); err != nil {
		return err
	}
	defer backend.operations.Done()
	return nativeStatusError(nativeOperationPostTCPReceive, backend.sdk.PostTCPReceive(backend.token, id, payload))
}

func (backend *sdkNativeBackend) CloseTCP(id intercept.NativeID) error {
	if err := backend.beginOperationWithoutContext(); err != nil {
		return err
	}
	defer backend.operations.Done()
	return nativeStatusError(nativeOperationCloseTCP, backend.sdk.CloseTCP(backend.token, id))
}

func (backend *sdkNativeBackend) PostUDPReceive(ctx context.Context, id intercept.NativeID, source netip.AddrPort, payload []byte) error {
	if !source.IsValid() || !source.Addr().Is4() || source.Port() == 0 {
		return fmt.Errorf("source %s: %w", source, ErrInvalidNativeAddress)
	}
	if err := backend.beginOperation(ctx); err != nil {
		return err
	}
	defer backend.operations.Done()
	return nativeStatusError(nativeOperationPostUDPReceive, backend.sdk.PostUDPReceive(backend.token, id, source, payload))
}

func (backend *sdkNativeBackend) SuspendUDP(id intercept.NativeID) error {
	if err := backend.beginOperationWithoutContext(); err != nil {
		return err
	}
	defer backend.operations.Done()
	return nativeStatusError(nativeOperationSuspendUDP, backend.sdk.SuspendUDP(backend.token, id))
}

func (backend *sdkNativeBackend) beginOperationWithoutContext() error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	switch backend.state {
	case nativeBackendStarting, nativeBackendStarted, nativeBackendFailed:
		backend.operations.Add(1)
		return nil
	case nativeBackendClosing, nativeBackendClosed:
		return ErrClosed
	default:
		return ErrNotStarted
	}
}

func (backend *sdkNativeBackend) beginOperation(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	backend.mu.Lock()
	switch backend.state {
	case nativeBackendStarting, nativeBackendStarted, nativeBackendFailed:
		backend.operations.Add(1)
		backend.mu.Unlock()
		if err := ctx.Err(); err != nil {
			backend.operations.Done()
			return err
		}
		return nil
	case nativeBackendClosing, nativeBackendClosed:
		backend.mu.Unlock()
		return ErrClosed
	default:
		backend.mu.Unlock()
		return ErrNotStarted
	}
}

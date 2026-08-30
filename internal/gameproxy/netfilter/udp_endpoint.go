package netfilter

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync"

	"bork/internal/gameproxy/intercept"
)

const (
	nativeUDPMaxPayload     = 65507
	nativeUDPQueueDatagrams = 256
)

type udpEndpoint struct {
	backend  nativeBackend
	metadata intercept.Metadata

	lifetime context.Context
	cancel   context.CancelCauseFunc
	incoming chan intercept.Datagram
	done     chan struct{}
	writer   chan struct{}

	stateMu     sync.Mutex
	terminal    error
	nativeEnded bool

	suspendOnce sync.Once
	suspendErr  error
}

var _ intercept.NativeUDPEndpoint = (*udpEndpoint)(nil)

func newUDPEndpoint(ctx context.Context, backend nativeBackend, metadata intercept.Metadata) *udpEndpoint {
	lifetime, cancel := context.WithCancelCause(ctx)
	writer := make(chan struct{}, 1)
	writer <- struct{}{}
	return &udpEndpoint{
		backend: backend, metadata: metadata, lifetime: lifetime, cancel: cancel,
		incoming: make(chan intercept.Datagram, nativeUDPQueueDatagrams),
		done:     make(chan struct{}), writer: writer,
	}
}

func (endpoint *udpEndpoint) Metadata() intercept.Metadata { return endpoint.metadata }

func (endpoint *udpEndpoint) ReadDatagram(ctx context.Context) (intercept.Datagram, error) {
	if err := ctx.Err(); err != nil {
		return intercept.Datagram{}, context.Cause(ctx)
	}
	if cause := endpoint.terminalCause(); cause != nil {
		return intercept.Datagram{}, cause
	}
	select {
	case <-ctx.Done():
		return intercept.Datagram{}, context.Cause(ctx)
	case <-endpoint.done:
		return intercept.Datagram{}, endpoint.terminalCause()
	case <-endpoint.lifetime.Done():
		return intercept.Datagram{}, context.Cause(endpoint.lifetime)
	case datagram := <-endpoint.incoming:
		if cause := endpoint.terminalCause(); cause != nil {
			return intercept.Datagram{}, cause
		}
		return datagram, nil
	}
}

func (endpoint *udpEndpoint) WriteDatagram(ctx context.Context, datagram intercept.Datagram) error {
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	if err := endpoint.validateWrite(datagram); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-endpoint.done:
		return endpoint.terminalCause()
	case <-endpoint.lifetime.Done():
		return context.Cause(endpoint.lifetime)
	case <-endpoint.writer:
	}
	defer func() { endpoint.writer <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	if cause := endpoint.terminalCause(); cause != nil {
		return cause
	}

	operationCtx, cancel := context.WithCancelCause(endpoint.lifetime)
	stop := context.AfterFunc(ctx, func() {
		cancel(context.Cause(ctx))
	})
	defer func() {
		stop()
		cancel(nil)
	}()
	err := endpoint.backend.PostUDPReceive(
		operationCtx, endpoint.metadata.NativeID, datagram.Metadata.OriginalRemote, slices.Clone(datagram.Payload),
	)
	if err == nil {
		return nil
	}
	if cause := endpoint.terminalCause(); cause != nil {
		return errors.Join(err, cause)
	}
	if ctx.Err() != nil {
		return errors.Join(err, context.Cause(ctx))
	}
	return err
}

func (endpoint *udpEndpoint) Reset(cause error) error {
	if cause == nil {
		cause = net.ErrClosed
	}
	return endpoint.terminate(cause)
}

func (endpoint *udpEndpoint) Close() error { return endpoint.terminate(net.ErrClosed) }

func (endpoint *udpEndpoint) enqueue(remote netip.AddrPort, payload []byte) error {
	if !validUDPDatagram(remote, payload) {
		return intercept.ErrInvalidFlow
	}
	metadata := endpoint.metadata
	metadata.OriginalRemote = remote
	datagram := intercept.Datagram{Metadata: metadata, Payload: slices.Clone(payload)}
	endpoint.stateMu.Lock()
	defer endpoint.stateMu.Unlock()
	if endpoint.terminal != nil || endpoint.nativeEnded {
		return net.ErrClosed
	}
	select {
	case endpoint.incoming <- datagram:
		return nil
	default:
		return intercept.ErrQueueFull
	}
}

func (endpoint *udpEndpoint) nativeClosed() {
	endpoint.stateMu.Lock()
	defer endpoint.stateMu.Unlock()
	if endpoint.terminal != nil || endpoint.nativeEnded {
		return
	}
	endpoint.nativeEnded = true
	endpoint.terminal = net.ErrClosed
	endpoint.cancel(net.ErrClosed)
	close(endpoint.done)
}

func (endpoint *udpEndpoint) terminate(cause error) error {
	endpoint.stateMu.Lock()
	if endpoint.terminal == nil {
		endpoint.terminal = cause
		endpoint.cancel(cause)
		close(endpoint.done)
	}
	needsSuspend := !endpoint.nativeEnded
	endpoint.stateMu.Unlock()
	if needsSuspend {
		<-endpoint.writer
		endpoint.writer <- struct{}{}
		endpoint.suspendOnce.Do(func() {
			endpoint.suspendErr = endpoint.backend.SuspendUDP(endpoint.metadata.NativeID)
		})
	}
	return endpoint.suspendErr
}

func (endpoint *udpEndpoint) terminalCause() error {
	endpoint.stateMu.Lock()
	defer endpoint.stateMu.Unlock()
	return endpoint.terminal
}

func (endpoint *udpEndpoint) validateWrite(datagram intercept.Datagram) error {
	metadata := datagram.Metadata
	fixed := metadata.Generation == endpoint.metadata.Generation &&
		metadata.NativeID == endpoint.metadata.NativeID && metadata.ProcessID == endpoint.metadata.ProcessID &&
		metadata.ExecutablePath == endpoint.metadata.ExecutablePath && metadata.OriginalLocal == endpoint.metadata.OriginalLocal
	if !fixed || !validIPv4Endpoint(metadata.OriginalRemote) || len(datagram.Payload) > nativeUDPMaxPayload {
		return &intercept.FlowError{
			NativeID: endpoint.metadata.NativeID, Operation: "validate native UDP receive", Cause: intercept.ErrInvalidFlow,
		}
	}
	return nil
}

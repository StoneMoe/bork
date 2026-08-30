package netfilter

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"sync"

	"bork/internal/gameproxy/intercept"
)

const (
	nativeTCPPacketBufferSize = 8192
	nativeTCPQueueChunks      = 256
)

type tcpFlow struct {
	backend  nativeBackend
	id       intercept.NativeID
	metadata intercept.Metadata

	writeCtx  context.Context
	cancel    context.CancelCauseFunc
	incoming  chan []byte
	inputDone chan struct{}
	done      chan struct{}

	readMu              sync.Mutex
	writeMu             sync.Mutex
	stateMu             sync.Mutex
	pending             []byte
	terminal            error
	nativeEnded         bool
	nativeCloseRequired bool

	nativeCloseOnce sync.Once
	nativeCloseErr  error
}

var _ intercept.NativeTCPFlow = (*tcpFlow)(nil)

func newTCPFlow(ctx context.Context, backend nativeBackend, metadata intercept.Metadata) *tcpFlow {
	writeCtx, cancel := context.WithCancelCause(ctx)
	return &tcpFlow{
		backend: backend, id: metadata.NativeID, metadata: metadata,
		writeCtx: writeCtx, cancel: cancel,
		incoming:  make(chan []byte, nativeTCPQueueChunks),
		inputDone: make(chan struct{}), done: make(chan struct{}),
	}
}

func (flow *tcpFlow) Metadata() intercept.Metadata { return flow.metadata }

func (flow *tcpFlow) Read(destination []byte) (int, error) {
	flow.readMu.Lock()
	defer flow.readMu.Unlock()
	if len(destination) == 0 {
		return 0, nil
	}
	if cause := flow.terminalCause(); cause != nil {
		return 0, cause
	}
	if len(flow.pending) > 0 {
		return flow.copyPending(destination), nil
	}

	payload, err := flow.nextPayload()
	if err != nil {
		return 0, err
	}
	if cause := flow.terminalCause(); cause != nil {
		return 0, cause
	}
	flow.pending = payload
	return flow.copyPending(destination), nil
}

func (flow *tcpFlow) Write(payload []byte) (int, error) {
	flow.writeMu.Lock()
	defer flow.writeMu.Unlock()
	if len(payload) == 0 {
		return 0, nil
	}
	written := 0
	for written < len(payload) {
		if err := flow.writeError(); err != nil {
			return written, err
		}
		end := min(written+nativeTCPPacketBufferSize, len(payload))
		if err := flow.backend.PostTCPReceive(flow.writeCtx, flow.id, payload[written:end]); err != nil {
			return written, errors.Join(err, flow.writeError())
		}
		written = end
	}
	return written, nil
}

func (flow *tcpFlow) Reset(cause error) error {
	if cause == nil {
		cause = net.ErrClosed
	}
	return flow.terminate(cause)
}

func (flow *tcpFlow) Close() error { return flow.terminate(net.ErrClosed) }

func (flow *tcpFlow) enqueue(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	owned := slices.Clone(payload)
	flow.stateMu.Lock()
	defer flow.stateMu.Unlock()
	if flow.terminal != nil || flow.nativeEnded {
		return net.ErrClosed
	}
	select {
	case flow.incoming <- owned:
		return nil
	default:
		return intercept.ErrQueueFull
	}
}

func (flow *tcpFlow) nativeClosed() {
	flow.stateMu.Lock()
	defer flow.stateMu.Unlock()
	if flow.nativeEnded {
		return
	}
	flow.nativeEnded = true
	flow.cancel(io.EOF)
	close(flow.inputDone)
}

func (flow *tcpFlow) terminate(cause error) error {
	flow.stateMu.Lock()
	if flow.terminal != nil {
		needsNativeClose := flow.nativeCloseRequired
		flow.stateMu.Unlock()
		if needsNativeClose {
			return flow.closeNative()
		}
		return nil
	}
	flow.terminal = cause
	flow.nativeCloseRequired = !flow.nativeEnded
	needsNativeClose := flow.nativeCloseRequired
	flow.cancel(cause)
	close(flow.done)
	flow.stateMu.Unlock()
	if needsNativeClose {
		return flow.closeNative()
	}
	return nil
}

func (flow *tcpFlow) closeNative() error {
	flow.nativeCloseOnce.Do(func() {
		flow.nativeCloseErr = flow.backend.CloseTCP(flow.id)
	})
	return flow.nativeCloseErr
}

func (flow *tcpFlow) terminalCause() error {
	flow.stateMu.Lock()
	defer flow.stateMu.Unlock()
	return flow.terminal
}

func (flow *tcpFlow) writeError() error {
	flow.stateMu.Lock()
	defer flow.stateMu.Unlock()
	if flow.terminal != nil {
		return flow.terminal
	}
	if flow.nativeEnded {
		return net.ErrClosed
	}
	return nil
}

func (flow *tcpFlow) nextPayload() ([]byte, error) {
	select {
	case <-flow.done:
		return nil, flow.terminalCause()
	default:
	}
	select {
	case payload := <-flow.incoming:
		return payload, nil
	default:
	}
	select {
	case <-flow.done:
		return nil, flow.terminalCause()
	case payload := <-flow.incoming:
		return payload, nil
	case <-flow.inputDone:
		select {
		case payload := <-flow.incoming:
			return payload, nil
		default:
			return nil, io.EOF
		}
	}
}

func (flow *tcpFlow) copyPending(destination []byte) int {
	written := copy(destination, flow.pending)
	flow.pending = flow.pending[written:]
	return written
}

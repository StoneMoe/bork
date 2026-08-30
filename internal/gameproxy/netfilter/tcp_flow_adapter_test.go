package netfilter

import (
	"bytes"
	"context"
	"errors"
	"net"
	"reflect"
	"runtime"
	"sync"
	"testing"

	"bork/internal/gameproxy/intercept"
)

func TestBridge_tcpSend_overflow_resets_only_full_flow(t *testing.T) {
	backend := &tcpTestBackend{}
	callbacks := &tcpTestCallbacks{
		state:    intercept.GenerationState{Generation: 5, Ready: true},
		accepted: make(chan tcpCallbackRecord, 2),
	}
	bridge := startTCPTestBridge(t, backend, callbacks)
	bridge.tcpConnected(validTCPEvent(101))
	first := (<-callbacks.accepted).flow
	bridge.tcpConnected(validTCPEvent(102))
	second := (<-callbacks.accepted).flow
	for range nativeTCPQueueChunks {
		bridge.tcpSend(101, []byte("x"))
	}

	bridge.tcpSend(101, []byte("overflow"))
	bridge.tcpSend(101, []byte("late"))
	bridge.tcpSend(102, []byte("ok"))
	_, firstErr := first.Read(make([]byte, 1))
	secondPayload := make([]byte, 2)
	secondN, secondErr := second.Read(secondPayload)

	if !errors.Is(firstErr, intercept.ErrQueueFull) {
		t.Fatalf("overflowed flow Read error = %v, want ErrQueueFull", firstErr)
	}
	if secondErr != nil || string(secondPayload[:secondN]) != "ok" {
		t.Fatalf("isolated flow Read = %q, %v, want ok", secondPayload[:secondN], secondErr)
	}
	if got := backend.closeSnapshot(); !reflect.DeepEqual(got, []intercept.NativeID{101}) {
		t.Fatalf("CloseTCP IDs = %v, want [101]", got)
	}
}

func TestTCPFlow_Write_splits_at_verified_SDK_limit(t *testing.T) {
	backend := &tcpTestBackend{}
	flow := acceptedTCPFlow(t, backend, 111)
	payload := bytes.Repeat([]byte("a"), nativeTCPPacketBufferSize*2+7)

	n, err := flow.Write(payload)

	if err != nil || n != len(payload) {
		t.Fatalf("Write = %d, %v, want %d, nil", n, err, len(payload))
	}
	calls := backend.receiveSnapshot()
	if len(calls) != 3 || len(calls[0].payload) != 8192 || len(calls[1].payload) != 8192 || len(calls[2].payload) != 7 {
		t.Fatalf("PostTCPReceive chunk sizes = %v", receiveSizes(calls))
	}
}

func TestTCPFlow_Write_returns_completed_bytes_before_post_failure(t *testing.T) {
	backend := &tcpTestBackend{postFailure: errTestPost, postFailureAt: 2}
	flow := acceptedTCPFlow(t, backend, 112)
	payload := bytes.Repeat([]byte("b"), nativeTCPPacketBufferSize+5)

	n, err := flow.Write(payload)

	if n != nativeTCPPacketBufferSize || !errors.Is(err, errTestPost) {
		t.Fatalf("Write = %d, %v, want %d and post failure", n, err, nativeTCPPacketBufferSize)
	}
}

func TestTCPFlow_Write_serializes_concurrent_writers(t *testing.T) {
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var once sync.Once
	backend := &tcpTestBackend{}
	backend.postHook = func(_ context.Context, _ intercept.NativeID, payload []byte) error {
		if payload[0] == 'a' {
			once.Do(func() {
				close(firstEntered)
				<-releaseFirst
			})
		}
		runtime.Gosched()
		return nil
	}
	flow := acceptedTCPFlow(t, backend, 117)
	firstResult := make(chan error, 1)
	secondStarted := make(chan struct{})
	secondResult := make(chan error, 1)
	go func() {
		_, err := flow.Write(bytes.Repeat([]byte("a"), nativeTCPPacketBufferSize+1))
		firstResult <- err
	}()
	<-firstEntered
	go func() {
		close(secondStarted)
		_, err := flow.Write([]byte("b"))
		secondResult <- err
	}()
	<-secondStarted
	for range 32 {
		runtime.Gosched()
	}

	close(releaseFirst)
	if err := <-firstResult; err != nil {
		t.Fatal(err)
	}
	if err := <-secondResult; err != nil {
		t.Fatal(err)
	}

	calls := backend.receiveSnapshot()
	if got := receiveInitials(calls); !reflect.DeepEqual(got, []byte{'a', 'a', 'b'}) {
		t.Fatalf("serialized call order = %q, want aab", got)
	}
}

func TestTCPFlow_Close_unblocks_Read_and_injectable_Write(t *testing.T) {
	backend := &tcpTestBackend{blockPosts: true, postStarted: make(chan struct{})}
	flow := acceptedTCPFlow(t, backend, 113)
	readResult := make(chan error, 1)
	writeResult := make(chan error, 1)
	go func() {
		_, err := flow.Read(make([]byte, 1))
		readResult <- err
	}()
	go func() {
		_, err := flow.Write([]byte("blocked"))
		writeResult <- err
	}()
	<-backend.postStarted

	closeErr := flow.Close()

	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if err := <-readResult; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("blocked Read error = %v, want net.ErrClosed", err)
	}
	if err := <-writeResult; !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked Write error = %v, want close cancellation", err)
	}
}

func TestBridge_Close_unblocks_active_flow_operations(t *testing.T) {
	backend := &tcpTestBackend{blockPosts: true, postStarted: make(chan struct{})}
	callbacks := &tcpTestCallbacks{
		state:    intercept.GenerationState{Generation: 6, Ready: true},
		accepted: make(chan tcpCallbackRecord, 1),
	}
	bridge := startTCPTestBridge(t, backend, callbacks)
	bridge.tcpConnected(validTCPEvent(118))
	flow := (<-callbacks.accepted).flow
	readResult := make(chan error, 1)
	writeResult := make(chan error, 1)
	go func() {
		_, err := flow.Read(make([]byte, 1))
		readResult <- err
	}()
	go func() {
		_, err := flow.Write([]byte("blocked"))
		writeResult <- err
	}()
	<-backend.postStarted

	closeErr := bridge.Close()

	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if err := <-readResult; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Bridge.Close Read error = %v, want net.ErrClosed", err)
	}
	if err := <-writeResult; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Bridge.Close Write error = %v, want net.ErrClosed", err)
	}
}

func TestTCPFlow_Reset_then_Close_performs_one_native_full_close_and_exposes_cause(t *testing.T) {
	backend := &tcpTestBackend{}
	flow := acceptedTCPFlow(t, backend, 114)
	cause := errors.New("relay reset")
	readResult := make(chan error, 1)
	go func() {
		_, err := flow.Read(make([]byte, 1))
		readResult <- err
	}()

	resetErr := flow.Reset(cause)
	closeErr := flow.Close()

	if resetErr != nil || closeErr != nil {
		t.Fatalf("Reset/Close errors = %v, %v", resetErr, closeErr)
	}
	if err := <-readResult; !errors.Is(err, cause) {
		t.Fatalf("blocked Read error = %v, want reset cause", err)
	}
	if got := backend.closeSnapshot(); !reflect.DeepEqual(got, []intercept.NativeID{114}) {
		t.Fatalf("CloseTCP IDs = %v, want one close for 114", got)
	}
}

func TestBridge_Close_closes_flows_before_backend_and_rejects_late_callback(t *testing.T) {
	backend := &tcpTestBackend{}
	callbacks := &tcpTestCallbacks{state: intercept.GenerationState{Generation: 7, Ready: true}}
	bridge := startTCPTestBridge(t, backend, callbacks)
	bridge.tcpConnected(validTCPEvent(115))

	if err := bridge.Close(); err != nil {
		t.Fatal(err)
	}
	bridge.tcpConnected(validTCPEvent(116))

	if got := backend.eventSnapshot(); !reflect.DeepEqual(got, []string{"tcp-close", "backend-close", "tcp-close"}) {
		t.Fatalf("close order = %v, want flow before backend and late rejection", got)
	}
	if _, calls := callbacks.counts(); calls != 1 {
		t.Fatalf("TCP callback calls = %d, want no callback after close", calls)
	}
	if got := backend.closeSnapshot(); !reflect.DeepEqual(got, []intercept.NativeID{115, 116}) {
		t.Fatalf("CloseTCP IDs = %v, want [115 116]", got)
	}
}

func acceptedTCPFlow(t *testing.T, backend *tcpTestBackend, id intercept.NativeID) intercept.NativeTCPFlow {
	t.Helper()
	callbacks := &tcpTestCallbacks{
		state:    intercept.GenerationState{Generation: 9, Ready: true},
		accepted: make(chan tcpCallbackRecord, 1),
	}
	bridge := startTCPTestBridge(t, backend, callbacks)
	bridge.tcpConnected(validTCPEvent(id))
	return (<-callbacks.accepted).flow
}

func receiveSizes(calls []tcpReceiveCall) []int {
	sizes := make([]int, len(calls))
	for index, call := range calls {
		sizes[index] = len(call.payload)
	}
	return sizes
}

func receiveInitials(calls []tcpReceiveCall) []byte {
	initials := make([]byte, len(calls))
	for index, call := range calls {
		initials[index] = call.payload[0]
	}
	return initials
}

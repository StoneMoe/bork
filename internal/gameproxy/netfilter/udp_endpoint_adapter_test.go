package netfilter

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"runtime"
	"sync"
	"testing"

	"bork/internal/gameproxy/intercept"
)

func TestUDPEndpoint_WriteDatagram_preserves_source_and_reports_backend_error(t *testing.T) {
	backend := &udpTestBackend{postErr: errTestUDPPost}
	metadata := udpTestMetadata(301)
	endpoint := newUDPEndpoint(context.Background(), backend, metadata)
	datagram := intercept.Datagram{Metadata: metadata, Payload: []byte("response")}

	err := endpoint.WriteDatagram(context.Background(), datagram)

	if !errors.Is(err, errTestUDPPost) {
		t.Fatalf("WriteDatagram error = %v, want backend failure", err)
	}
	calls := backend.receiveSnapshot()
	if len(calls) != 1 || calls[0].id != metadata.NativeID || calls[0].source != metadata.OriginalRemote || string(calls[0].payload) != "response" {
		t.Fatalf("PostUDPReceive calls = %+v, want exact ID, source, and payload", calls)
	}
}

func TestUDPEndpoint_WriteDatagram_rejects_changed_identity_local_generation_and_invalid_source(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*intercept.Metadata)
	}{
		{name: "generation", mutate: func(metadata *intercept.Metadata) { metadata.Generation++ }},
		{name: "native ID", mutate: func(metadata *intercept.Metadata) { metadata.NativeID++ }},
		{name: "process ID", mutate: func(metadata *intercept.Metadata) { metadata.ProcessID++ }},
		{name: "path", mutate: func(metadata *intercept.Metadata) { metadata.ExecutablePath = `c:\changed.exe` }},
		{name: "local", mutate: func(metadata *intercept.Metadata) {
			metadata.OriginalLocal = netip.MustParseAddrPort("10.0.0.3:42000")
		}},
		{name: "source", mutate: func(metadata *intercept.Metadata) {
			metadata.OriginalRemote = netip.MustParseAddrPort("[2001:db8::1]:9000")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &udpTestBackend{}
			metadata := udpTestMetadata(302)
			endpoint := newUDPEndpoint(context.Background(), backend, metadata)
			datagramMetadata := metadata
			test.mutate(&datagramMetadata)

			err := endpoint.WriteDatagram(context.Background(), intercept.Datagram{Metadata: datagramMetadata, Payload: []byte("x")})

			if !errors.Is(err, intercept.ErrInvalidFlow) {
				t.Fatalf("WriteDatagram error = %v, want ErrInvalidFlow", err)
			}
			if calls := backend.receiveSnapshot(); len(calls) != 0 {
				t.Fatalf("PostUDPReceive calls = %+v, want none", calls)
			}
		})
	}
}

func TestUDPEndpoint_WriteDatagram_serializes_writers(t *testing.T) {
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var once sync.Once
	backend := &udpTestBackend{}
	backend.postHook = func(_ context.Context, _ intercept.NativeID, _ netip.AddrPort, payload []byte) error {
		if payload[0] == 'a' {
			once.Do(func() {
				close(firstEntered)
				<-releaseFirst
			})
		}
		return nil
	}
	metadata := udpTestMetadata(303)
	endpoint := newUDPEndpoint(context.Background(), backend, metadata)
	firstResult := make(chan error, 1)
	secondStarted := make(chan struct{})
	secondResult := make(chan error, 1)
	go func() {
		firstResult <- endpoint.WriteDatagram(context.Background(), intercept.Datagram{Metadata: metadata, Payload: []byte("a")})
	}()
	<-firstEntered
	go func() {
		close(secondStarted)
		secondResult <- endpoint.WriteDatagram(context.Background(), intercept.Datagram{Metadata: metadata, Payload: []byte("b")})
	}()
	<-secondStarted
	for range 32 {
		runtime.Gosched()
	}
	if calls := backend.receiveSnapshot(); len(calls) != 1 {
		t.Fatalf("concurrent PostUDPReceive calls = %d, want 1", len(calls))
	}

	close(releaseFirst)
	if err := <-firstResult; err != nil {
		t.Fatal(err)
	}
	if err := <-secondResult; err != nil {
		t.Fatal(err)
	}
	calls := backend.receiveSnapshot()
	if got := []string{string(calls[0].payload), string(calls[1].payload)}; !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("serialized payloads = %v, want [a b]", got)
	}
}

func TestUDPEndpoint_operations_respond_to_context_and_close(t *testing.T) {
	backend := &udpTestBackend{postStarted: make(chan struct{})}
	backend.postHook = func(ctx context.Context, _ intercept.NativeID, _ netip.AddrPort, _ []byte) error {
		<-ctx.Done()
		return context.Cause(ctx)
	}
	metadata := udpTestMetadata(304)
	endpoint := newUDPEndpoint(context.Background(), backend, metadata)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := endpoint.ReadDatagram(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ReadDatagram error = %v, want context.Canceled", err)
	}
	if err := endpoint.WriteDatagram(canceled, intercept.Datagram{Metadata: metadata, Payload: []byte("canceled")}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled WriteDatagram error = %v, want context.Canceled", err)
	}
	if calls := backend.receiveSnapshot(); len(calls) != 0 {
		t.Fatalf("canceled write reached backend: %+v", calls)
	}
	readResult := make(chan error, 1)
	writeResult := make(chan error, 1)
	go func() {
		_, err := endpoint.ReadDatagram(context.Background())
		readResult <- err
	}()
	go func() {
		writeResult <- endpoint.WriteDatagram(context.Background(), intercept.Datagram{Metadata: metadata, Payload: []byte("blocked")})
	}()
	<-backend.postStarted

	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}

	if err := <-readResult; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("blocked read error = %v, want net.ErrClosed", err)
	}
	if err := <-writeResult; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("blocked write error = %v, want net.ErrClosed", err)
	}
}

func TestUDPEndpoint_Reset_then_Close_suspends_once_and_exposes_cause(t *testing.T) {
	backend := &udpTestBackend{}
	endpoint := newUDPEndpoint(context.Background(), backend, udpTestMetadata(305))
	cause := errors.New("relay reset")

	resetErr := endpoint.Reset(cause)
	closeErr := endpoint.Close()
	_, readErr := endpoint.ReadDatagram(context.Background())

	if resetErr != nil || closeErr != nil {
		t.Fatalf("Reset/Close errors = %v, %v", resetErr, closeErr)
	}
	if !errors.Is(readErr, cause) {
		t.Fatalf("ReadDatagram error = %v, want reset cause", readErr)
	}
	if got := backend.suspendSnapshot(); !reflect.DeepEqual(got, []intercept.NativeID{305}) {
		t.Fatalf("SuspendUDP IDs = %v, want [305]", got)
	}
}

func TestBridge_Close_closes_UDP_endpoints_and_pending_sockets_before_backend(t *testing.T) {
	backend := &udpTestBackend{}
	callbacks := &udpTestCallbacks{
		state: intercept.GenerationState{Generation: 5, Ready: true}, accepted: make(chan udpCallbackRecord, 1),
	}
	bridge := startUDPTestBridge(t, backend, callbacks)
	bridge.udpCreated(validUDPEvent(311))
	bridge.udpSend(validUDPSend(311, netip.MustParseAddrPort("8.8.8.8:9000"), []byte("first")))
	endpoint := (<-callbacks.accepted).endpoint
	if _, err := endpoint.ReadDatagram(context.Background()); err != nil {
		t.Fatal(err)
	}
	bridge.udpCreated(validUDPEvent(312))
	readResult := make(chan error, 1)
	go func() {
		_, err := endpoint.ReadDatagram(context.Background())
		readResult <- err
	}()

	if err := bridge.Close(); err != nil {
		t.Fatal(err)
	}

	if err := <-readResult; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("blocked read error = %v, want net.ErrClosed", err)
	}
	events := backend.eventSnapshot()
	if len(events) != 3 || events[2] != "backend-close" {
		t.Fatalf("close events = %v, want two suspends before backend close", events)
	}
	if got := backend.suspendSnapshot(); !sameNativeIDs(got, []intercept.NativeID{311, 312}) {
		t.Fatalf("SuspendUDP IDs = %v, want 311 and 312", got)
	}
}

func udpTestMetadata(id intercept.NativeID) intercept.Metadata {
	return intercept.Metadata{
		Generation: 8, NativeID: id, ProcessID: 51, ExecutablePath: `c:\games\game.exe`,
		OriginalLocal:  netip.MustParseAddrPort("10.0.0.2:42000"),
		OriginalRemote: netip.MustParseAddrPort("203.0.113.8:9000"),
	}
}

func sameNativeIDs(got, want []intercept.NativeID) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[intercept.NativeID]int, len(got))
	for _, id := range got {
		seen[id]++
	}
	for _, id := range want {
		seen[id]--
	}
	for _, count := range seen {
		if count != 0 {
			return false
		}
	}
	return true
}

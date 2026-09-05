package netfilter

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"reflect"
	"testing"

	"bork/internal/gameproxy/intercept"
)

type startContextKey struct{}

func TestBridge_tcpConnected_stamps_one_generation_snapshot_and_exact_metadata(t *testing.T) {
	backend := &tcpTestBackend{}
	callbacks := &tcpTestCallbacks{
		state:    intercept.GenerationState{Generation: 17, Ready: true},
		accepted: make(chan tcpCallbackRecord, 1),
	}
	ctx := context.WithValue(context.Background(), startContextKey{}, "start")
	bridge := newTestBridge(t, backend)
	if err := bridge.Start(ctx, callbacks); err != nil {
		t.Fatal(err)
	}
	event := validTCPEvent(71)

	bridge.tcpConnected(event)
	record := <-callbacks.accepted

	if record.ctx != ctx {
		t.Fatal("TCP callback context is not the Bridge.Start context")
	}
	want := intercept.Metadata{
		Generation: 17, NativeID: 71, ProcessID: 41,
		ExecutablePath: event.ExecutablePath,
		OriginalLocal:  event.Local, OriginalRemote: event.Remote,
	}
	if got := record.flow.Metadata(); got != want {
		t.Fatalf("flow metadata = %+v, want %+v", got, want)
	}
	event.ExecutablePath = `c:\changed.exe`
	callbacks.state.Generation = 18
	if got := record.flow.Metadata(); got != want {
		t.Fatalf("metadata changed after admission: %+v", got)
	}
	if generations, calls := callbacks.counts(); generations != 1 || calls != 1 {
		t.Fatalf("callback counts = generation %d, TCP %d, want 1 and 1", generations, calls)
	}
}

func TestBridge_tcpConnected_rejects_not_ready_invalid_duplicate_and_admission_error(t *testing.T) {
	t.Run("not ready", func(t *testing.T) {
		backend := &tcpTestBackend{}
		callbacks := &tcpTestCallbacks{state: intercept.GenerationState{Generation: 2}}
		bridge := startTCPTestBridge(t, backend, callbacks)

		bridge.tcpConnected(validTCPEvent(81))

		if got := backend.closeSnapshot(); !reflect.DeepEqual(got, []intercept.NativeID{81}) {
			t.Fatalf("CloseTCP IDs = %v, want [81]", got)
		}
		if generations, calls := callbacks.counts(); generations != 1 || calls != 0 {
			t.Fatalf("callback counts = generation %d, TCP %d, want 1 and 0", generations, calls)
		}
	})

	t.Run("invalid endpoint and path", func(t *testing.T) {
		backend := &tcpTestBackend{}
		callbacks := &tcpTestCallbacks{state: intercept.GenerationState{Generation: 2, Ready: true}}
		bridge := startTCPTestBridge(t, backend, callbacks)
		invalidEndpoint := validTCPEvent(82)
		invalidEndpoint.Remote = netip.MustParseAddrPort("[2001:db8::1]:443")
		invalidPath := validTCPEvent(83)
		invalidPath.ExecutablePath = `game.exe`

		bridge.tcpConnected(invalidEndpoint)
		bridge.tcpConnected(invalidPath)

		if got := backend.closeSnapshot(); !reflect.DeepEqual(got, []intercept.NativeID{82, 83}) {
			t.Fatalf("CloseTCP IDs = %v, want [82 83]", got)
		}
		if _, calls := callbacks.counts(); calls != 0 {
			t.Fatalf("TCP callback calls = %d, want 0", calls)
		}
	})

	t.Run("duplicate ID", func(t *testing.T) {
		backend := &tcpTestBackend{}
		callbacks := &tcpTestCallbacks{state: intercept.GenerationState{Generation: 2, Ready: true}}
		bridge := startTCPTestBridge(t, backend, callbacks)

		bridge.tcpConnected(validTCPEvent(84))
		bridge.tcpConnected(validTCPEvent(84))

		if got := backend.closeSnapshot(); !reflect.DeepEqual(got, []intercept.NativeID{84}) {
			t.Fatalf("CloseTCP IDs = %v, want one close for 84", got)
		}
		if _, calls := callbacks.counts(); calls != 1 {
			t.Fatalf("TCP callback calls = %d, want 1", calls)
		}
	})

	t.Run("callback admission error", func(t *testing.T) {
		admissionErr := errors.New("admission rejected")
		backend := &tcpTestBackend{}
		callbacks := &tcpTestCallbacks{
			state:        intercept.GenerationState{Generation: 2, Ready: true},
			admissionErr: admissionErr, accepted: make(chan tcpCallbackRecord, 1),
		}
		bridge := startTCPTestBridge(t, backend, callbacks)

		bridge.tcpConnected(validTCPEvent(85))
		flow := (<-callbacks.accepted).flow
		_, readErr := flow.Read(make([]byte, 1))

		if !errors.Is(readErr, admissionErr) {
			t.Fatalf("Read error = %v, want admission error", readErr)
		}
		if got := backend.closeSnapshot(); !reflect.DeepEqual(got, []intercept.NativeID{85}) {
			t.Fatalf("CloseTCP IDs = %v, want [85]", got)
		}
	})
}

func TestBridge_tcpSend_copies_borrowed_bytes_and_preserves_partial_reads(t *testing.T) {
	backend := &tcpTestBackend{}
	callbacks := &tcpTestCallbacks{
		state:    intercept.GenerationState{Generation: 3, Ready: true},
		accepted: make(chan tcpCallbackRecord, 1),
	}
	bridge := startTCPTestBridge(t, backend, callbacks)
	bridge.tcpConnected(validTCPEvent(91))
	flow := (<-callbacks.accepted).flow
	payload := []byte("abcd")

	bridge.tcpSend(91, payload)
	copy(payload, "zzzz")
	first := make([]byte, 2)
	second := make([]byte, 4)
	firstN, firstErr := flow.Read(first)
	secondN, secondErr := flow.Read(second)

	if firstErr != nil || secondErr != nil || string(first[:firstN])+string(second[:secondN]) != "abcd" {
		t.Fatalf("partial reads = %q/%v then %q/%v", first[:firstN], firstErr, second[:secondN], secondErr)
	}
	if _, ok := flow.(interface{ CloseRead() error }); ok {
		t.Fatal("native flow unexpectedly advertises CloseRead")
	}
	if _, ok := flow.(interface{ CloseWrite() error }); ok {
		t.Fatal("native flow unexpectedly advertises CloseWrite")
	}
}

func TestBridge_tcpClosed_drains_queue_then_returns_EOF_without_native_close(t *testing.T) {
	backend := &tcpTestBackend{}
	callbacks := &tcpTestCallbacks{
		state:    intercept.GenerationState{Generation: 4, Ready: true},
		accepted: make(chan tcpCallbackRecord, 1),
	}
	bridge := startTCPTestBridge(t, backend, callbacks)
	bridge.tcpConnected(validTCPEvent(92))
	flow := (<-callbacks.accepted).flow
	bridge.tcpSend(92, []byte("queued"))

	bridge.tcpClosed(92)
	payload, err := io.ReadAll(flow)

	if err != nil || string(payload) != "queued" {
		t.Fatalf("ReadAll = %q, %v, want queued then EOF", payload, err)
	}
	if got := backend.closeSnapshot(); len(got) != 0 {
		t.Fatalf("CloseTCP IDs = %v, want none", got)
	}
}

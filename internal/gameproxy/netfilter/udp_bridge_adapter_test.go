package netfilter

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"testing"

	"bork/internal/gameproxy/intercept"
)

func TestBridge_UDP_lazily_admits_with_first_owned_datagram_and_immutable_metadata(t *testing.T) {
	backend := &udpTestBackend{}
	callbacks := &udpTestCallbacks{
		state: intercept.GenerationState{Generation: 17, Ready: true}, accepted: make(chan udpCallbackRecord, 1),
	}
	firstRead := make(chan intercept.Datagram, 1)
	callbacks.onUDP = func(_ context.Context, endpoint intercept.NativeUDPEndpoint) error {
		datagram, err := endpoint.ReadDatagram(context.Background())
		if err != nil {
			return err
		}
		firstRead <- datagram
		return nil
	}
	ctx := context.WithValue(context.Background(), startContextKey{}, "udp-start")
	bridge := newTestBridge(t, backend)
	if err := bridge.Start(ctx, callbacks); err != nil {
		t.Fatal(err)
	}
	event := validUDPEvent(201)
	bridge.udpCreated(event)
	if generations, calls := callbacks.counts(); generations != 0 || calls != 0 {
		t.Fatalf("callbacks after udpCreated = generation %d, UDP %d, want zero", generations, calls)
	}
	remote := netip.MustParseAddrPort("8.8.8.8:9000")
	payload := []byte("first")

	bridge.udpSend(validUDPSend(event.ID, remote, payload))
	copy(payload, "xxxxx")
	first := <-firstRead
	record := <-callbacks.accepted

	bridge.mu.Lock()
	endpoint := bridge.udpEndpoints[event.ID]
	bridge.mu.Unlock()
	if endpoint == nil {
		t.Fatal("UDP endpoint was not registered")
	}
	if record.ctx != ctx || record.endpoint != endpoint {
		t.Fatal("UDP admission did not use the Bridge.Start context and registered endpoint")
	}
	wantBase := intercept.Metadata{
		Generation: 17, NativeID: event.ID, ProcessID: event.PID,
		ExecutablePath: event.ExecutablePath, OriginalLocal: event.Local, OriginalRemote: remote,
	}
	if got := endpoint.Metadata(); got != wantBase {
		t.Fatalf("endpoint metadata = %+v, want %+v", got, wantBase)
	}
	if string(first.Payload) != "first" || first.Metadata != wantBase {
		t.Fatalf("first datagram = %+v %q, want owned first datagram", first.Metadata, first.Payload)
	}
	callbacks.setState(intercept.GenerationState{Generation: 99, Ready: true})
	secondRemote := netip.MustParseAddrPort("203.0.113.8:7777")
	bridge.udpSend(validUDPSend(event.ID, secondRemote, []byte("second")))
	second, err := endpoint.ReadDatagram(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.Metadata.OriginalRemote != secondRemote || second.Metadata.Generation != 17 {
		t.Fatalf("second metadata = %+v, want remote %v and generation 17", second.Metadata, secondRemote)
	}
	if endpoint.Metadata() != wantBase {
		t.Fatal("base metadata changed after subsequent datagram")
	}
	if generations, calls := callbacks.counts(); generations != 1 || calls != 1 {
		t.Fatalf("callback counts = generation %d, UDP %d, want 1 and 1", generations, calls)
	}
}

func TestBridge_UDP_rejects_unsafe_events_fail_closed(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Bridge)
		id   intercept.NativeID
	}{
		{name: "send before create", id: 211, run: func(bridge *Bridge) {
			bridge.udpSend(validUDPSend(211, netip.MustParseAddrPort("8.8.8.8:53"), []byte("x")))
		}},
		{name: "invalid created local", id: 212, run: func(bridge *Bridge) {
			event := validUDPEvent(212)
			event.Local = netip.MustParseAddrPort("[2001:db8::1]:4000")
			bridge.udpCreated(event)
		}},
		{name: "invalid remote", id: 213, run: func(bridge *Bridge) {
			bridge.udpCreated(validUDPEvent(213))
			bridge.udpSend(validUDPSend(213, netip.MustParseAddrPort("[2001:db8::1]:53"), []byte("x")))
		}},
		{name: "oversized payload", id: 214, run: func(bridge *Bridge) {
			bridge.udpCreated(validUDPEvent(214))
			bridge.udpSend(validUDPSend(214, netip.MustParseAddrPort("8.8.8.8:53"), bytes.Repeat([]byte("x"), 65508)))
		}},
		{name: "duplicate created ID", id: 215, run: func(bridge *Bridge) {
			bridge.udpCreated(validUDPEvent(215))
			bridge.udpCreated(validUDPEvent(215))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &udpTestBackend{}
			callbacks := &udpTestCallbacks{state: intercept.GenerationState{Generation: 1, Ready: true}}
			bridge := startUDPTestBridge(t, backend, callbacks)

			test.run(bridge)

			if got := backend.suspendSnapshot(); !reflect.DeepEqual(got, []intercept.NativeID{test.id}) {
				t.Fatalf("SuspendUDP IDs = %v, want [%d]", got, test.id)
			}
			if _, calls := callbacks.counts(); calls != 0 {
				t.Fatalf("UDP callbacks = %d, want 0", calls)
			}
		})
	}
}

func TestBridge_UDP_rejects_not_ready_and_admission_error(t *testing.T) {
	t.Run("not ready", func(t *testing.T) {
		backend := &udpTestBackend{}
		callbacks := &udpTestCallbacks{state: intercept.GenerationState{Generation: 2}}
		bridge := startUDPTestBridge(t, backend, callbacks)
		bridge.udpCreated(validUDPEvent(221))

		bridge.udpSend(validUDPSend(221, netip.MustParseAddrPort("8.8.8.8:53"), []byte("query")))

		if got := backend.suspendSnapshot(); !reflect.DeepEqual(got, []intercept.NativeID{221}) {
			t.Fatalf("SuspendUDP IDs = %v, want [221]", got)
		}
		if generations, calls := callbacks.counts(); generations != 1 || calls != 0 {
			t.Fatalf("callbacks = generation %d, UDP %d, want 1 and 0", generations, calls)
		}
	})

	t.Run("admission error", func(t *testing.T) {
		admissionErr := errors.New("UDP admission rejected")
		backend := &udpTestBackend{}
		callbacks := &udpTestCallbacks{
			state: intercept.GenerationState{Generation: 2, Ready: true}, admissionErr: admissionErr,
			accepted: make(chan udpCallbackRecord, 1),
		}
		bridge := startUDPTestBridge(t, backend, callbacks)
		bridge.udpCreated(validUDPEvent(222))

		bridge.udpSend(validUDPSend(222, netip.MustParseAddrPort("8.8.8.8:53"), []byte("query")))
		endpoint := (<-callbacks.accepted).endpoint
		_, readErr := endpoint.ReadDatagram(context.Background())

		if !errors.Is(readErr, admissionErr) {
			t.Fatalf("ReadDatagram error = %v, want admission error", readErr)
		}
		if got := backend.suspendSnapshot(); !reflect.DeepEqual(got, []intercept.NativeID{222}) {
			t.Fatalf("SuspendUDP IDs = %v, want [222]", got)
		}
	})
}

func TestBridge_udpSend_overflow_isolated_to_one_endpoint(t *testing.T) {
	backend := &udpTestBackend{}
	callbacks := &udpTestCallbacks{
		state: intercept.GenerationState{Generation: 3, Ready: true}, accepted: make(chan udpCallbackRecord, 2),
	}
	bridge := startUDPTestBridge(t, backend, callbacks)
	remote := netip.MustParseAddrPort("8.8.8.8:9000")
	bridge.udpCreated(validUDPEvent(231))
	bridge.udpSend(validUDPSend(231, remote, []byte("first")))
	first := (<-callbacks.accepted).endpoint
	bridge.udpCreated(validUDPEvent(232))
	bridge.udpSend(validUDPSend(232, remote, []byte("ok")))
	second := (<-callbacks.accepted).endpoint
	for range nativeUDPQueueDatagrams - 1 {
		bridge.udpSend(validUDPSend(231, remote, []byte("fill")))
	}

	bridge.udpSend(validUDPSend(231, remote, []byte("overflow")))
	_, firstErr := first.ReadDatagram(context.Background())
	secondDatagram, secondErr := second.ReadDatagram(context.Background())

	if !errors.Is(firstErr, intercept.ErrQueueFull) {
		t.Fatalf("overflowed endpoint error = %v, want ErrQueueFull", firstErr)
	}
	if secondErr != nil || string(secondDatagram.Payload) != "ok" {
		t.Fatalf("isolated endpoint read = %q, %v, want ok", secondDatagram.Payload, secondErr)
	}
	if got := backend.suspendSnapshot(); !reflect.DeepEqual(got, []intercept.NativeID{231}) {
		t.Fatalf("SuspendUDP IDs = %v, want [231]", got)
	}
}

func TestBridge_udpClosed_unblocks_without_suspend(t *testing.T) {
	backend := &udpTestBackend{}
	callbacks := &udpTestCallbacks{
		state: intercept.GenerationState{Generation: 4, Ready: true}, accepted: make(chan udpCallbackRecord, 1),
	}
	bridge := startUDPTestBridge(t, backend, callbacks)
	bridge.udpCreated(validUDPEvent(241))
	bridge.udpSend(validUDPSend(241, netip.MustParseAddrPort("8.8.8.8:9000"), []byte("first")))
	endpoint := (<-callbacks.accepted).endpoint
	if _, err := endpoint.ReadDatagram(context.Background()); err != nil {
		t.Fatal(err)
	}
	readResult := make(chan error, 1)
	go func() {
		_, err := endpoint.ReadDatagram(context.Background())
		readResult <- err
	}()

	bridge.udpClosed(241)

	if err := <-readResult; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("blocked read error = %v, want net.ErrClosed", err)
	}
	if got := backend.suspendSnapshot(); len(got) != 0 {
		t.Fatalf("SuspendUDP IDs = %v, want none", got)
	}
}

func TestBridge_UDP_duplicate_active_and_callbacks_after_close_fail_closed(t *testing.T) {
	backend := &udpTestBackend{}
	callbacks := &udpTestCallbacks{
		state: intercept.GenerationState{Generation: 6, Ready: true}, accepted: make(chan udpCallbackRecord, 1),
	}
	bridge := startUDPTestBridge(t, backend, callbacks)
	bridge.udpCreated(validUDPEvent(251))
	bridge.udpSend(validUDPSend(251, netip.MustParseAddrPort("8.8.8.8:9000"), []byte("first")))
	endpoint := (<-callbacks.accepted).endpoint

	bridge.udpCreated(validUDPEvent(251))
	_, duplicateErr := endpoint.ReadDatagram(context.Background())
	if !errors.Is(duplicateErr, intercept.ErrDuplicateFlow) {
		t.Fatalf("duplicate endpoint error = %v, want ErrDuplicateFlow", duplicateErr)
	}
	if err := bridge.Close(); err != nil {
		t.Fatal(err)
	}
	bridge.udpCreated(validUDPEvent(252))
	bridge.udpSend(validUDPSend(253, netip.MustParseAddrPort("8.8.8.8:9000"), []byte("late")))

	if got := backend.suspendSnapshot(); !reflect.DeepEqual(got, []intercept.NativeID{251, 252, 253}) {
		t.Fatalf("SuspendUDP IDs = %v, want [251 252 253]", got)
	}
}

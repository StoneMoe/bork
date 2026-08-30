package intercept

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"
)

func TestRelay_UDP_rewrites_DNS_and_reverse_maps_source(t *testing.T) {
	packet := newFakePacketConn()
	packet.writeGate = make(chan struct{})
	dialer := &fakeDialer{packetConn: packet}
	relay := newTestRelay(t, fakeRules{selected: true}, dialer)
	relay.SetState(GenerationState{Generation: 12, Ready: true})
	endpoint := newFakeUDPEndpoint(testMetadata(12, 31, 53))
	if err := relay.UDP(context.Background(), endpoint); err != nil {
		t.Fatal(err)
	}
	payload := []byte("dns query")
	datagram := Datagram{Metadata: endpoint.metadata, Payload: payload}

	endpoint.reads <- datagram
	stackBuffer := waitBytes(t, packet.writeStarted)
	payload[0] = 'X'
	close(packet.writeGate)
	written := waitPacketWrite(t, packet.writes)
	response := []byte("dns response")
	packet.reads <- packetRead{payload: response, source: netip.MustParseAddrPort("1.1.1.1:53")}
	injected := waitDatagram(t, endpoint.writes)

	if written.destination != netip.MustParseAddrPort("1.1.1.1:53") {
		t.Fatalf("UDP destination = %v, want 1.1.1.1:53", written.destination)
	}
	if string(stackBuffer) != "dns query" || string(written.payload) != "dns query" {
		t.Fatalf("stack payloads = %q and %q, want copied dns query", stackBuffer, written.payload)
	}
	if injected.Metadata.OriginalRemote != netip.MustParseAddrPort("8.8.8.8:53") {
		t.Fatalf("native source = %v, want original 8.8.8.8:53", injected.Metadata.OriginalRemote)
	}
	if string(injected.Payload) != "dns response" {
		t.Fatalf("native payload = %q, want dns response", injected.Payload)
	}
}

func TestRelay_UDP_rejects_unselected_and_stale_endpoints_without_opening_stack(t *testing.T) {
	unselected := testMetadata(20, 41, 9000)
	unselected.ExecutablePath = "C:/tools/helper.exe"
	tests := []struct {
		name      string
		rules     fakeRules
		state     GenerationState
		metadata  Metadata
		wantError error
	}{
		{
			name: "unselected executable", rules: fakeRules{selected: false},
			state:    GenerationState{Generation: 20, Ready: true},
			metadata: unselected, wantError: ErrUnselected,
		},
		{
			name: "stale generation", rules: fakeRules{selected: true},
			state:    GenerationState{Generation: 21, Ready: true},
			metadata: testMetadata(20, 42, 9000), wantError: ErrStaleGeneration,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dialer := &fakeDialer{}
			relay := newTestRelay(t, test.rules, dialer)
			relay.SetState(test.state)
			endpoint := newFakeUDPEndpoint(test.metadata)

			err := relay.UDP(context.Background(), endpoint)

			if !errors.Is(err, test.wantError) {
				t.Fatalf("UDP() error = %v, want %v", err, test.wantError)
			}
			if dialer.udpCallCount() != 0 {
				t.Fatal("OpenUDP was called for rejected endpoint")
			}
		})
	}
}

func TestRelay_UDP_preserves_non_DNS_destination_per_datagram(t *testing.T) {
	packet := newFakePacketConn()
	dialer := &fakeDialer{packetConn: packet}
	relay := newTestRelay(t, fakeRules{selected: true}, dialer)
	relay.SetState(GenerationState{Generation: 13, Ready: true})
	endpoint := newFakeUDPEndpoint(testMetadata(13, 32, 9000))
	if err := relay.UDP(context.Background(), endpoint); err != nil {
		t.Fatal(err)
	}
	metadata := endpoint.metadata
	metadata.OriginalRemote = netip.MustParseAddrPort("203.0.113.8:7777")

	endpoint.reads <- Datagram{Metadata: metadata, Payload: []byte("game packet")}
	written := waitPacketWrite(t, packet.writes)

	if written.destination != metadata.OriginalRemote {
		t.Fatalf("UDP destination = %v, want %v", written.destination, metadata.OriginalRemote)
	}
}

func TestRelay_UDP_copies_stack_buffers_and_serializes_native_writes(t *testing.T) {
	packet := newFakePacketConn()
	dialer := &fakeDialer{packetConn: packet}
	relay := newTestRelay(t, fakeRules{selected: true}, dialer)
	relay.SetState(GenerationState{Generation: 17, Ready: true})
	endpoint := newFakeUDPEndpoint(testMetadata(17, 36, 9000))
	endpoint.writeGate = make(chan struct{})
	if err := relay.UDP(context.Background(), endpoint); err != nil {
		t.Fatal(err)
	}

	packet.reads <- packetRead{payload: []byte("first"), source: netip.MustParseAddrPort("203.0.113.1:9000")}
	first := waitDatagram(t, endpoint.writeStarted)
	packet.reads <- packetRead{payload: []byte("other"), source: netip.MustParseAddrPort("203.0.113.2:9000")}
	waitEvent(t, packet.readObserved)
	waitEvent(t, packet.readObserved)

	if string(first.Payload) != "first" {
		t.Fatalf("first native payload = %q after stack buffer reuse, want first", first.Payload)
	}
	select {
	case <-endpoint.writeStarted:
		t.Fatal("native writes ran concurrently")
	default:
	}
	close(endpoint.writeGate)
}

func TestRelay_UDP_closes_endpoint_when_queue_overflows(t *testing.T) {
	packet := newFakePacketConn()
	packet.writeGate = make(chan struct{})
	dialer := &fakeDialer{packetConn: packet}
	relay := newTestRelayWithQueue(t, dialer, 1)
	relay.SetState(GenerationState{Generation: 14, Ready: true})
	endpoint := newFakeUDPEndpoint(testMetadata(14, 33, 9000))
	if err := relay.UDP(context.Background(), endpoint); err != nil {
		t.Fatal(err)
	}

	endpoint.reads <- Datagram{Metadata: endpoint.metadata, Payload: []byte("first")}
	waitBytes(t, packet.writeStarted)
	endpoint.reads <- Datagram{Metadata: endpoint.metadata, Payload: []byte("second")}
	waitEvent(t, endpoint.readObserved)
	endpoint.reads <- Datagram{Metadata: endpoint.metadata, Payload: []byte("third")}
	waitEvent(t, endpoint.readObserved)
	resetErr := waitError(t, endpoint.reset)
	close(packet.writeGate)

	if !errors.Is(resetErr, ErrQueueFull) {
		t.Fatalf("reset error = %v, want ErrQueueFull", resetErr)
	}
}

func TestRelay_UDP_closes_endpoint_on_packet_error(t *testing.T) {
	packetFailure := errors.New("packet write failed")
	packet := newFakePacketConn()
	packet.writeErr = packetFailure
	dialer := &fakeDialer{packetConn: packet}
	relay := newTestRelay(t, fakeRules{selected: true}, dialer)
	relay.SetState(GenerationState{Generation: 15, Ready: true})
	endpoint := newFakeUDPEndpoint(testMetadata(15, 34, 9000))
	if err := relay.UDP(context.Background(), endpoint); err != nil {
		t.Fatal(err)
	}

	endpoint.reads <- Datagram{Metadata: endpoint.metadata, Payload: []byte("packet")}
	resetErr := waitError(t, endpoint.reset)

	if !errors.Is(resetErr, ErrPacket) || !errors.Is(resetErr, packetFailure) {
		t.Fatalf("reset error = %v, want ErrPacket wrapping packet error", resetErr)
	}
}

func TestRelay_ExpireIdle_closes_idle_UDP_endpoint(t *testing.T) {
	packet := newFakePacketConn()
	dialer := &fakeDialer{packetConn: packet, udpCalled: make(chan struct{})}
	relay := newTestRelay(t, fakeRules{selected: true}, dialer)
	relay.SetState(GenerationState{Generation: 16, Ready: true})
	endpoint := newFakeUDPEndpoint(testMetadata(16, 35, 9000))
	if err := relay.UDP(context.Background(), endpoint); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, dialer.udpCalled)

	expired := relay.ExpireIdle(time.Unix(161, 0))
	resetErr := waitError(t, endpoint.reset)

	if expired != 1 {
		t.Fatalf("ExpireIdle() = %d, want 1", expired)
	}
	if !errors.Is(resetErr, ErrIdle) {
		t.Fatalf("reset error = %v, want ErrIdle", resetErr)
	}
}

func newTestRelayWithQueue(t *testing.T, dialer Dialer, queueSize int) *Relay {
	t.Helper()
	relay, err := New(Options{
		Bridge: noopBridge{}, Rules: fakeRules{selected: true}, Dialer: dialer,
		DNS: netip.MustParseAddr("1.1.1.1"), QueueSize: queueSize,
		IdleTimeout: time.Minute, Clock: &fakeClock{now: time.Unix(100, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { relay.Close() })
	return relay
}

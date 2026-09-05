package intercept

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestRelay_UDP_rewrites_DNS_and_reverse_maps_source(t *testing.T) {
	packet := newFakePacketConn()
	packet.writeGate = make(chan struct{})
	dialer := newDNSFakeDialer(packet)
	relay := newTestRelay(t, fakeRules{selected: true}, dialer)
	events := make(chan FlowEvent, 3)
	relay.options.OnFlowEvent = func(event FlowEvent) {
		if event.Kind == FlowEventRequest || event.Kind == FlowEventResponse || event.Kind == FlowEventComplete {
			events <- event
		}
	}
	relay.SetState(GenerationState{Generation: 12, Ready: true})
	endpoint := newFakeUDPEndpoint(testMetadata(12, 31, 53))
	if err := relay.UDP(context.Background(), endpoint); err != nil {
		t.Fatal(err)
	}
	payload := testDNSPacket(0x1234, false, 'a')
	wantQuery := append([]byte(nil), payload...)
	datagram := Datagram{Metadata: endpoint.metadata, Payload: payload}

	endpoint.reads <- datagram
	stackBuffer := waitBytes(t, packet.writeStarted)
	payload[0] ^= 0xff
	close(packet.writeGate)
	written := waitPacketWrite(t, packet.writes)
	response := testDNSPacket(0x1234, true, 'a')
	packet.reads <- packetRead{payload: response, source: netip.MustParseAddrPort("1.1.1.1:53")}
	injected := waitDatagram(t, endpoint.writes)
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}

	if written.destination != netip.MustParseAddrPort("1.1.1.1:53") {
		t.Fatalf("UDP destination = %v, want 1.1.1.1:53", written.destination)
	}
	if !bytes.Equal(stackBuffer, wantQuery) || !bytes.Equal(written.payload, wantQuery) {
		t.Fatalf("stack payloads = %x and %x, want copied DNS query %x", stackBuffer, written.payload, wantQuery)
	}
	if injected.Metadata.OriginalRemote != netip.MustParseAddrPort("8.8.8.8:53") {
		t.Fatalf("native source = %v, want original 8.8.8.8:53", injected.Metadata.OriginalRemote)
	}
	if !bytes.Equal(injected.Payload, response) {
		t.Fatalf("native payload = %x, want DNS response %x", injected.Payload, response)
	}
	request, serverResponse, complete := <-events, <-events, <-events
	if request.Kind != FlowEventRequest || request.RequestID != "udp-31-dns-1" || request.Bytes != uint64(len(wantQuery)) {
		t.Fatalf("UDP request event = %#v", request)
	}
	if serverResponse.Kind != FlowEventResponse || serverResponse.RequestID != request.RequestID ||
		serverResponse.Bytes != uint64(len(response)) {
		t.Fatalf("UDP response event = %#v", serverResponse)
	}
	if complete.Kind != FlowEventComplete || complete.FlowID != "udp-31" ||
		complete.UploadPackets != 1 || complete.DownloadPackets != 1 ||
		complete.UploadBytes != uint64(len(wantQuery)) || complete.DownloadBytes != uint64(len(response)) {
		t.Fatalf("UDP complete event = %#v", complete)
	}
	traffic := relay.Traffic()
	if traffic.UploadBytes != uint64(len(wantQuery)) || traffic.DownloadBytes != uint64(len(response)) {
		t.Fatalf("traffic = %#v, want one count per directional UDP payload", traffic)
	}
}

func TestRelay_UDP_correlates_reordered_DNS_responses_by_server_and_transaction(t *testing.T) {
	firstPacket := newFakePacketConn()
	secondPacket := newFakePacketConn()
	relay := newTestRelay(t, fakeRules{selected: true}, newDNSFakeDialer(firstPacket, secondPacket))
	events := make(chan FlowEvent, 4)
	relay.options.OnFlowEvent = func(event FlowEvent) {
		if event.Kind == FlowEventRequest || event.Kind == FlowEventResponse {
			events <- event
		}
	}
	relay.SetState(GenerationState{Generation: 12, Ready: true})
	endpoint := newFakeUDPEndpoint(testMetadata(12, 37, 53))
	if err := relay.UDP(context.Background(), endpoint); err != nil {
		t.Fatal(err)
	}
	first := endpoint.metadata
	first.OriginalRemote = netip.MustParseAddrPort("8.8.8.8:53")
	second := endpoint.metadata
	second.OriginalRemote = netip.MustParseAddrPort("9.9.9.9:53")
	endpoint.reads <- Datagram{Metadata: first, Payload: testDNSPacket(0x1234, false, 'a')}
	endpoint.reads <- Datagram{Metadata: second, Payload: testDNSPacket(0x1234, false, 'b')}
	waitPacketWrite(t, firstPacket.writes)
	waitPacketWrite(t, secondPacket.writes)

	secondPacket.reads <- packetRead{payload: testDNSPacket(0x1234, true, 'b'), source: netip.MustParseAddrPort("1.1.1.1:53")}
	firstPacket.reads <- packetRead{payload: testDNSPacket(0x1234, true, 'a'), source: netip.MustParseAddrPort("1.1.1.1:53")}
	for range 2 {
		response := waitDatagram(t, endpoint.writes)
		wantSource := first.OriginalRemote
		if response.Payload[13] == 'b' {
			wantSource = second.OriginalRemote
		}
		if response.Metadata.OriginalRemote != wantSource {
			t.Fatalf("response source = %v, want %v for payload %x", response.Metadata.OriginalRemote, wantSource, response.Payload)
		}
	}

	requests := make(map[netip.AddrPort]string)
	responses := make(map[netip.AddrPort]string)
	for range 4 {
		event := <-events
		if event.Kind == FlowEventRequest {
			requests[event.Metadata.OriginalRemote] = event.RequestID
		} else {
			responses[event.Metadata.OriginalRemote] = event.RequestID
		}
	}
	for _, remote := range []netip.AddrPort{first.OriginalRemote, second.OriginalRemote} {
		if requests[remote] == "" || responses[remote] != requests[remote] {
			t.Fatalf("flow IDs for %v = request %q, response %q", remote, requests[remote], responses[remote])
		}
	}
}

func TestRelay_UDP_reverse_maps_expired_DNS_response_to_original_server(t *testing.T) {
	packet := newFakePacketConn()
	dialer := newDNSFakeDialer(packet)
	dialer.udpCalled = make(chan struct{})
	relay := newTestRelay(t, fakeRules{selected: true}, dialer)
	relay.SetState(GenerationState{Generation: 12, Ready: true})
	endpoint := newFakeUDPEndpoint(testMetadata(12, 42, 53))
	if err := relay.UDP(context.Background(), endpoint); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, dialer.udpCalled)
	endpoint.reads <- Datagram{Metadata: endpoint.metadata, Payload: testDNSPacket(0x1234, false, 'a')}
	waitPacketWrite(t, packet.writes)
	clock := relay.options.Clock.(*fakeClock)
	clock.mu.Lock()
	clock.now = clock.now.Add(udpTraceRequestTimeout + time.Second)
	clock.mu.Unlock()

	packet.reads <- packetRead{payload: testDNSPacket(0x1234, true, 'a'), source: netip.MustParseAddrPort("1.1.1.1:53")}
	injected := waitDatagram(t, endpoint.writes)
	if injected.Metadata.OriginalRemote != endpoint.metadata.OriginalRemote {
		t.Fatalf("expired DNS source = %v, want %v", injected.Metadata.OriginalRemote, endpoint.metadata.OriginalRemote)
	}
}

func TestUDPSession_duplicate_DNS_transactions_match_socket_original_source(t *testing.T) {
	relay := newTestRelay(t, fakeRules{selected: true}, &fakeDialer{})
	session := udpSession{relay: relay, metadata: testMetadata(12, 38, 53)}
	server := netip.MustParseAddrPort("1.1.1.1:53")
	first := netip.MustParseAddrPort("8.8.8.8:53")
	second := netip.MustParseAddrPort("9.9.9.9:53")
	query := testDNSPacket(0x1234, false, 'a')
	response := testDNSPacket(0x1234, true, 'a')
	firstRequest, firstAdded := session.addDNSTraceRequest(first, server, query)
	secondRequest, secondAdded := session.addDNSTraceRequest(second, server, query)
	secondResponse, secondMatched := session.takeDNSTraceRequest(second, server, response)
	firstResponse, firstMatched := session.takeDNSTraceRequest(first, server, response)
	if !firstAdded || !secondAdded || !firstMatched || !secondMatched ||
		firstResponse.id != firstRequest.id || firstResponse.originalRemote != first ||
		secondResponse.id != secondRequest.id || secondResponse.originalRemote != second {
		t.Fatalf("duplicate transaction responses = %#v, %#v", firstResponse, secondResponse)
	}
}

func TestUDPSession_coalesces_DNS_retransmissions_before_transaction_reuse(t *testing.T) {
	relay := newTestRelay(t, fakeRules{selected: true}, &fakeDialer{})
	session := udpSession{relay: relay, metadata: testMetadata(12, 38, 53)}
	original := netip.MustParseAddrPort("8.8.8.8:53")
	resolver := netip.MustParseAddrPort("1.1.1.1:53")
	query := testDNSPacket(0x1234, false, 'a')
	first, ok := session.addDNSTraceRequest(original, resolver, query)
	if !ok {
		t.Fatal("first query was not correlated")
	}
	clock := relay.options.Clock.(*fakeClock)
	clock.mu.Lock()
	clock.now = clock.now.Add(time.Second)
	clock.mu.Unlock()
	retry, ok := session.addDNSTraceRequest(original, resolver, query)
	if !ok || retry.id != first.id || len(session.tracePending) != 1 || !retry.createdAt.After(first.createdAt) {
		t.Fatalf("retransmission = %#v, pending = %#v", retry, session.tracePending)
	}
	response, ok := session.takeDNSTraceRequest(original, resolver, testDNSPacket(0x1234, true, 'a'))
	if !ok || response.id != first.id || len(session.tracePending) != 0 {
		t.Fatalf("response = %#v, pending = %#v", response, session.tracePending)
	}
	next, ok := session.addDNSTraceRequest(original, resolver, testDNSPacket(0x1234, false, 'b'))
	if !ok || next.id == first.id {
		t.Fatalf("reused transaction = %#v, want a new request ID", next)
	}
	response, ok = session.takeDNSTraceRequest(original, resolver, testDNSPacket(0x1234, true, 'b'))
	if !ok || response.id != next.id {
		t.Fatalf("reused transaction response = %#v, want request %q", response, next.id)
	}
}

func TestUDPSession_does_not_create_request_correlation_for_generic_datagrams(t *testing.T) {
	relay := newTestRelay(t, fakeRules{selected: true}, &fakeDialer{})
	session := udpSession{relay: relay, metadata: testMetadata(12, 39, 9000)}
	remote := netip.MustParseAddrPort("203.0.113.8:9000")
	if request, ok := session.addDNSTraceRequest(remote, remote, []byte("request")); ok || request != (pendingUDPTraceRequest{}) {
		t.Fatalf("generic trace request = %#v, %t", request, ok)
	}
	if response, ok := session.takeDNSTraceRequest(remote, remote, []byte("response")); ok || response != (pendingUDPTraceRequest{}) {
		t.Fatalf("generic trace response = %#v, %t", response, ok)
	}
}

func TestUDPSession_rejects_malformed_or_wrong_direction_DNS_correlation(t *testing.T) {
	relay := newTestRelay(t, fakeRules{selected: true}, &fakeDialer{})
	session := udpSession{relay: relay, metadata: testMetadata(12, 43, 53)}
	original := netip.MustParseAddrPort("8.8.8.8:53")
	resolver := netip.MustParseAddrPort("1.1.1.1:53")
	for _, payload := range [][]byte{{0x12, 0x34}, testDNSPacket(0x1234, true, 'a')} {
		if request, ok := session.addDNSTraceRequest(original, resolver, payload); ok || request != (pendingUDPTraceRequest{}) {
			t.Fatalf("invalid DNS query correlation = %#v, %t", request, ok)
		}
	}
	request, ok := session.addDNSTraceRequest(original, resolver, testDNSPacket(0x1234, false, 'a'))
	if !ok {
		t.Fatal("valid DNS query was not correlated")
	}
	if response, ok := session.takeDNSTraceRequest(original, resolver, testDNSPacket(0x1234, false, 'a')); ok || response != (pendingUDPTraceRequest{}) {
		t.Fatalf("DNS query accepted as response = %#v, %t", response, ok)
	}
	if response, ok := session.takeDNSTraceRequest(original, resolver, testDNSPacket(0x1234, true, 'a')); !ok || response.id != request.id {
		t.Fatalf("valid DNS response correlation = %#v, %t", response, ok)
	}
}

func TestUDPSession_limits_DNS_upstream_sockets(t *testing.T) {
	relay := newTestRelay(t, fakeRules{selected: true}, &fakeDialer{})
	session := udpSession{
		relay: relay, metadata: testMetadata(12, 44, 53),
		dnsPacketConns: make(map[netip.AddrPort]net.PacketConn),
	}
	packetConn := newFakePacketConn()
	for index := 1; index <= maxDNSPacketConns; index++ {
		remote := netip.AddrPortFrom(netip.AddrFrom4([4]byte{192, 0, 2, byte(index)}), 53)
		session.dnsPacketConns[remote] = packetConn
	}

	_, err := session.openDNSPacketConn(
		context.Background(), netip.MustParseAddrPort("198.51.100.1:53"),
		make(chan queuedNativeDatagram), make(chan error, 1),
	)

	if !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("DNS socket limit error = %v, want ErrResourceLimit", err)
	}
}

func TestUDPSession_reports_bounded_DNS_correlation_drops(t *testing.T) {
	relay := newTestRelay(t, fakeRules{selected: true}, &fakeDialer{})
	events := make(chan FlowEvent, 1)
	relay.options.OnFlowEvent = func(event FlowEvent) { events <- event }
	session := udpSession{relay: relay, metadata: testMetadata(12, 45, 53), flowID: "udp-45"}
	original := netip.MustParseAddrPort("8.8.8.8:53")
	resolver := netip.MustParseAddrPort("1.1.1.1:53")
	for index := range maxPendingUDPTraceRequests + 1 {
		session.addDNSTraceRequest(original, resolver, testDNSPacket(uint16(index), false, 'a'))
	}

	session.emitDroppedDNSTraces()
	event := <-events

	if len(session.tracePending) != maxPendingUDPTraceRequests || event.Kind != FlowEventDropped ||
		event.RequestID != "udp-45-dns-correlation" || event.Bytes != 1 {
		t.Fatalf("DNS correlation drop event = %#v with %d pending", event, len(session.tracePending))
	}
}

func TestRelay_UDP_logs_only_first_generic_datagram_in_each_direction(t *testing.T) {
	packet := newFakePacketConn()
	relay := newTestRelay(t, fakeRules{selected: true}, &fakeDialer{packetConn: packet})
	events := make(chan FlowEvent, 4)
	relay.options.OnFlowEvent = func(event FlowEvent) {
		if event.Kind == FlowEventOutbound || event.Kind == FlowEventInbound {
			events <- event
		}
	}
	relay.SetState(GenerationState{Generation: 12, Ready: true})
	endpoint := newFakeUDPEndpoint(testMetadata(12, 40, 9000))
	if err := relay.UDP(context.Background(), endpoint); err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{"first outbound", "second outbound"} {
		endpoint.reads <- Datagram{Metadata: endpoint.metadata, Payload: []byte(payload)}
		waitPacketWrite(t, packet.writes)
	}
	for _, payload := range []string{"first inbound", "second inbound"} {
		packet.reads <- packetRead{payload: []byte(payload), source: endpoint.metadata.OriginalRemote}
		waitDatagram(t, endpoint.writes)
	}

	outbound, inbound := <-events, <-events
	if outbound.Kind != FlowEventOutbound || outbound.FlowID != "udp-40" || outbound.Bytes != uint64(len("first outbound")) {
		t.Fatalf("first outbound event = %#v", outbound)
	}
	if inbound.Kind != FlowEventInbound || inbound.FlowID != outbound.FlowID || inbound.Bytes != uint64(len("first inbound")) {
		t.Fatalf("first inbound event = %#v", inbound)
	}
	select {
	case event := <-events:
		t.Fatalf("unexpected repeated directional event = %#v", event)
	default:
	}
}

func TestUDPSession_emits_changed_generic_traffic_as_interval_delta(t *testing.T) {
	relay := newTestRelay(t, fakeRules{selected: true}, &fakeDialer{})
	events := make(chan FlowEvent, 3)
	relay.options.OnFlowEvent = func(event FlowEvent) {
		if event.Kind == FlowEventTraffic {
			events <- event
		}
	}
	session := udpSession{relay: relay, metadata: testMetadata(12, 41, 9000), flowID: "udp-41"}
	for range 3 {
		session.recordGenericUpload(session.metadata, 100)
	}
	for range 2 {
		session.recordGenericDownload(session.metadata, 250)
	}

	session.emitGenericTraffic()
	first := <-events
	if first.Kind != FlowEventTraffic || first.FlowID != "udp-41" ||
		first.UploadPackets != 3 || first.UploadBytes != 300 || first.DownloadPackets != 2 || first.DownloadBytes != 500 {
		t.Fatalf("first traffic event = %#v", first)
	}
	session.recordGenericUpload(session.metadata, 40)
	session.emitGenericTraffic()
	second := <-events
	if second.UploadPackets != 1 || second.UploadBytes != 40 || second.DownloadPackets != 0 || second.DownloadBytes != 0 {
		t.Fatalf("second traffic event = %#v", second)
	}
	session.emitGenericTraffic()
	select {
	case event := <-events:
		t.Fatalf("unchanged traffic event = %#v", event)
	default:
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
	failedEvents := make(chan FlowEvent, 1)
	relay.options.OnFlowEvent = func(event FlowEvent) {
		if event.Kind == FlowEventComplete && event.Err != nil {
			failedEvents <- event
		}
	}
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
	failed := <-failedEvents
	if failed.FlowID != "udp-34" || !errors.Is(failed.Err, packetFailure) {
		t.Fatalf("failed event = %#v, want matching UDP flow", failed)
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

func newDNSFakeDialer(packetConns ...net.PacketConn) *fakeDialer {
	return &fakeDialer{packetConns: append([]net.PacketConn{newFakePacketConn()}, packetConns...)}
}

func testDNSPacket(transactionID uint16, response bool, label byte) []byte {
	payload := make([]byte, 19)
	binary.BigEndian.PutUint16(payload, transactionID)
	if response {
		payload[2] = 0x80
	}
	binary.BigEndian.PutUint16(payload[4:6], 1)
	payload[12] = 1
	payload[13] = label
	payload[15] = 0
	payload[16] = 1
	payload[17] = 0
	payload[18] = 1
	return payload
}

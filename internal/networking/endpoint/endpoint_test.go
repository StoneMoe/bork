package endpoint

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"syscall"
	"testing"
	"time"
)

func TestEndpointUsesOneSocketForSTUN(t *testing.T) {
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP() error = %v", err)
	}
	defer server.Close()
	seenClient := make(chan netip.AddrPort, 1)
	go func() {
		buffer := make([]byte, 512)
		count, client, readErr := server.ReadFromUDPAddrPort(buffer)
		if readErr != nil {
			return
		}
		transaction, ok := stunTransactionFromMessage(buffer[:count])
		if !ok {
			return
		}
		seenClient <- client
		_, _ = server.WriteToUDPAddrPort(bindingResponse(transaction, client), client)
	}()

	endpoint := New(Options{
		ListenAddress: "0.0.0.0:0",
		STUNServers:   []string{server.LocalAddr().String()},
		STUNTimeout:   time.Second,
		STUNRefresh:   0,
	}, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- endpoint.Run(ctx) }()

	var snapshot Snapshot
	deadline := time.After(3 * time.Second)
	for snapshot.STUN == nil || len(snapshot.STUN) == 0 || snapshot.STUN[0].MappedAddress == "" {
		select {
		case _, ok := <-endpoint.SnapshotChanges():
			if !ok {
				t.Fatal("endpoint stopped before STUN result")
			}
			snapshot = endpoint.Snapshot()
		case <-deadline:
			t.Fatalf("timed out waiting for STUN result: %#v", endpoint.Snapshot())
		}
	}
	client := <-seenClient
	listenAddress := netip.MustParseAddrPort(snapshot.ListenAddress)
	if listenAddress.Port() != client.Port() {
		t.Fatalf("listen port = %d, STUN source port = %d", listenAddress.Port(), client.Port())
	}
	if snapshot.STUN[0].MappedAddress != client.String() {
		t.Fatalf("mapped address = %q, want %q", snapshot.STUN[0].MappedAddress, client)
	}
	if !hasCandidate(snapshot.Candidates, CandidateServerReflexive, client.String()) {
		t.Fatalf("server-reflexive candidate missing: %#v", snapshot.Candidates)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("endpoint did not stop")
	}
}

func TestEndpointSTUNTimeoutIsNonFatal(t *testing.T) {
	unused, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP() error = %v", err)
	}
	serverAddress := unused.LocalAddr().String()
	_ = unused.Close()

	endpoint := New(Options{
		ListenAddress: "127.0.0.1:0",
		STUNServers:   []string{serverAddress},
		STUNTimeout:   20 * time.Millisecond,
		STUNRefresh:   0,
	}, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- endpoint.Run(ctx) }()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-endpoint.SnapshotChanges():
			snapshot := endpoint.Snapshot()
			if len(snapshot.STUN) == 1 && snapshot.STUN[0].Error != "" {
				cancel()
				if err := <-done; err != nil {
					t.Fatalf("Run() error = %v", err)
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for STUN timeout result")
		}
	}
}

func TestEndpointsExchangePeerDatagrams(t *testing.T) {
	first := New(Options{ListenAddress: "127.0.0.1:0", STUNServers: []string{}, STUNRefresh: 0}, testLogger())
	second := New(Options{ListenAddress: "127.0.0.1:0", STUNServers: []string{}, STUNRefresh: 0}, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() { firstDone <- first.Run(ctx) }()
	go func() { secondDone <- second.Run(ctx) }()
	firstAddress := waitForListenAddress(t, first)
	secondAddress := waitForListenAddress(t, second)

	payload := []byte("bork peer datagram")
	if err := first.Send(payload, secondAddress); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	select {
	case packet := <-second.ControlPackets():
		if string(packet.Data) != string(payload) {
			t.Fatalf("packet data = %q", packet.Data)
		}
		if packet.From.Port() != firstAddress.Port() {
			t.Fatalf("packet source port = %d, want %d", packet.From.Port(), firstAddress.Port())
		}
		if packet.ReceivedAt.IsZero() {
			t.Fatal("packet receipt time was not recorded at the socket")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for peer datagram")
	}

	cancel()
	if err := <-firstDone; err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
}

func TestEndpointDropsOversizedDatagramAndContinues(t *testing.T) {
	endpoint := New(Options{ListenAddress: "127.0.0.1:0", STUNServers: []string{}, STUNRefresh: 0}, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- endpoint.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run() error = %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("endpoint did not stop")
		}
	}()

	address := waitForListenAddress(t, endpoint)
	sender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP() error = %v", err)
	}
	defer sender.Close()
	if _, err := sender.WriteToUDPAddrPort(make([]byte, maxDatagramSize+1), address); err != nil {
		t.Fatalf("write oversized datagram: %v", err)
	}
	want := []byte("valid after oversized")
	if _, err := sender.WriteToUDPAddrPort(want, address); err != nil {
		t.Fatalf("write valid datagram: %v", err)
	}

	select {
	case packet, ok := <-endpoint.ControlPackets():
		if !ok {
			t.Fatal("endpoint stopped after oversized datagram")
		}
		if !bytes.Equal(packet.Data, want) {
			t.Fatalf("received datagram length = %d, want valid datagram length %d", len(packet.Data), len(want))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for valid datagram")
	}
}

func TestIPv4FallbackOnlyHandlesUnsupportedAddressFamily(t *testing.T) {
	if !unsupportedAddressFamily(syscall.EAFNOSUPPORT) || !unsupportedAddressFamily(syscall.EPROTONOSUPPORT) {
		t.Fatal("address-family errors did not enable IPv4 fallback")
	}
	if unsupportedAddressFamily(syscall.EADDRINUSE) {
		t.Fatal("address-in-use error enabled IPv4 fallback")
	}
}

func TestEndpointUsesControlLaneForOpaqueDatagrams(t *testing.T) {
	endpoint := New(Options{ListenAddress: "127.0.0.1:0", STUNServers: []string{}, STUNRefresh: 0}, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- endpoint.Run(ctx) }()
	address := waitForListenAddress(t, endpoint)
	sender, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	voiceLooking := []byte{'B', 'R', 'K', '2', 2, 4}
	if _, err := sender.WriteToUDPAddrPort(voiceLooking, address); err != nil {
		t.Fatal(err)
	}
	select {
	case packet := <-endpoint.ControlPackets():
		if string(packet.Data) != string(voiceLooking) {
			t.Fatalf("packet = %x", packet.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for packet")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestVoiceQueueDropsWholeOldestBatch(t *testing.T) {
	endpoint := New(Options{}, testLogger())
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	endpoint.conn = conn
	destination := netip.MustParseAddrPort("127.0.0.1:9000")
	endpoint.InvalidateVoice(1)
	first := VoiceBatch{Generation: 1, Datagrams: []VoiceDatagram{
		{Data: []byte{1}, Destination: destination},
		{Data: []byte{2}, Destination: destination},
	}}
	second := VoiceBatch{Generation: 1, Datagrams: []VoiceDatagram{{Data: []byte{3}, Destination: destination}}}
	if err := endpoint.SendVoiceBatch(first); err != nil {
		t.Fatal(err)
	}
	if err := endpoint.SendVoiceBatch(second); err != nil {
		t.Fatal(err)
	}
	queued := <-endpoint.voiceBatches
	if len(queued.Datagrams) != 1 || queued.Datagrams[0].Data[0] != 3 {
		t.Fatalf("queued voice batch = %#v", queued)
	}
	if err := endpoint.SendVoiceBatch(second); err != nil {
		t.Fatal(err)
	}
	endpoint.InvalidateVoice(2)
	if len(endpoint.voiceBatches) != 0 {
		t.Fatal("voice invalidation left a queued batch")
	}
	if err := endpoint.SendVoiceBatch(second); err == nil {
		t.Fatal("stale voice generation was accepted")
	}
}

func TestControlIngressLimiterBoundsEachSourceAndTrackedState(t *testing.T) {
	limiter := newIngressLimiter(controlSourceRate, controlSourceBurst, controlGlobalRate, controlGlobalBurst, maxControlSources)
	now := time.Unix(1000, 0)
	source := netip.MustParseAddrPort("192.0.2.1:0")
	for range int(controlSourceBurst) {
		if !limiter.allow(source, now) {
			t.Fatal("control limiter rejected a packet within the source burst")
		}
	}
	if limiter.allow(source, now) {
		t.Fatal("control limiter accepted a packet above the source burst")
	}
	if !limiter.allow(source, now.Add(time.Second)) {
		t.Fatal("control limiter did not replenish source tokens")
	}
	for index := 1; index <= maxControlSources+20; index++ {
		address := netip.AddrPortFrom(netip.AddrFrom4([4]byte{198, 51, byte(index / 256), byte(index % 256)}), 0)
		limiter.allow(address, now.Add(time.Duration(index)*time.Millisecond))
	}
	if len(limiter.sources) != maxControlSources {
		t.Fatalf("tracked control sources = %d, want %d", len(limiter.sources), maxControlSources)
	}
}

func TestVoiceIngressLimiterBoundsOnePath(t *testing.T) {
	limiter := newIngressLimiter(voiceSourceRate, voiceSourceBurst, voiceGlobalRate, voiceGlobalBurst, maxVoiceSources)
	now := time.Unix(1000, 0)
	source := netip.MustParseAddrPort("192.0.2.1:9000")
	for range int(voiceSourceBurst) {
		if !limiter.allow(source, now) {
			t.Fatal("voice limiter rejected a packet within the path burst")
		}
	}
	if limiter.allow(source, now) {
		t.Fatal("voice limiter accepted a packet above the path burst")
	}
	if !limiter.allow(source, now.Add(time.Second)) {
		t.Fatal("voice limiter did not replenish path tokens")
	}
}

func TestVoiceIPLimiterCannotBeBypassedWithSourcePorts(t *testing.T) {
	limiter := newIngressLimiter(voiceIPRate, voiceIPBurst, voiceGlobalRate, voiceGlobalBurst, maxVoiceSources)
	now := time.Unix(1000, 0)
	address := netip.MustParseAddr("192.0.2.1")
	for index := range int(voiceIPBurst) {
		source := netip.AddrPortFrom(address, uint16(index+1))
		ipSource := netip.AddrPortFrom(source.Addr(), 0)
		if !limiter.allow(ipSource, now) {
			t.Fatal("voice IP limiter rejected a packet within the IP burst")
		}
	}
	if limiter.allow(netip.AddrPortFrom(address, 0), now) {
		t.Fatal("rotating source ports bypassed the voice IP burst")
	}
}

func TestWriteDeadlineBoundsGateWait(t *testing.T) {
	endpoint := New(Options{}, testLogger())
	endpoint.writeGate <- struct{}{}
	defer endpoint.releaseWrite()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	started := time.Now()
	err = endpoint.writeWithDeadline(context.Background(), conn, []byte{1}, netip.MustParseAddrPort("127.0.0.1:9000"), started.Add(20*time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("writeWithDeadline() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("write gate ignored deadline for %v", elapsed)
	}
}

func waitForListenAddress(t *testing.T, endpoint *Endpoint) netip.AddrPort {
	t.Helper()
	select {
	case <-endpoint.SnapshotChanges():
		snapshot := endpoint.Snapshot()
		address, err := netip.ParseAddrPort(snapshot.ListenAddress)
		if err != nil {
			t.Fatalf("ParseAddrPort() error = %v", err)
		}
		return address
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for listen address")
		return netip.AddrPort{}
	}
}

func hasCandidate(candidates []Candidate, candidateType CandidateType, address string) bool {
	for _, candidate := range candidates {
		if candidate.Type == candidateType && candidate.Address == address {
			return true
		}
	}
	return false
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

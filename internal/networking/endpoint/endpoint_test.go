package endpoint

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"testing"
	"time"

	"bork/internal/protocol"

	"golang.org/x/crypto/chacha20poly1305"
)

var endpointTestRoomTag = [16]byte{1}

func TestDefaultOptionsDoNotSelectPublicSTUNProviders(t *testing.T) {
	options := DefaultOptions()
	if options.STUNServers == nil || len(options.STUNServers) != 0 {
		t.Fatalf("DefaultOptions().STUNServers = %#v, want empty", options.STUNServers)
	}
	normalized := normalizeOptions(Options{})
	if normalized.STUNServers == nil || len(normalized.STUNServers) != 0 {
		t.Fatalf("normalizeOptions(Options{}).STUNServers = %#v, want empty", normalized.STUNServers)
	}
}

func TestSnapshotCloneDoesNotAliasSlices(t *testing.T) {
	snapshot := Snapshot{
		Candidates: []Candidate{{Address: "192.0.2.1:9000"}},
		STUN:       []STUNResult{{Server: "stun.example:3478"}},
	}
	clone := snapshot.Clone()
	clone.Candidates[0].Address = "changed"
	clone.STUN[0].Server = "changed"

	if snapshot.Candidates[0].Address != "192.0.2.1:9000" || snapshot.STUN[0].Server != "stun.example:3478" {
		t.Fatalf("clone mutation reached endpoint snapshot: %#v", snapshot)
	}
}

func TestRealtimeGenerationInvalidationPreservesExternalBatches(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	endpointUDP := New(Options{}, endpointTestRoomTag, testLogger())
	endpointUDP.mu.Lock()
	endpointUDP.conn = conn
	endpointUDP.mu.Unlock()
	destination := conn.LocalAddr().(*net.UDPAddr).AddrPort()
	endpointUDP.InvalidateRealtime(1)
	for _, batch := range []RealtimeBatch{
		{Class: protocol.TrafficAudio, Generation: 1, Datagrams: []RealtimeDatagram{{Data: []byte{1}, Destination: destination}}},
		{Class: protocol.TrafficAudio, Datagrams: []RealtimeDatagram{{Data: []byte{2}, Destination: destination}}},
		{Class: protocol.TrafficInteractive, Generation: 1, Datagrams: []RealtimeDatagram{{Data: []byte{3}, Destination: destination}}},
		{Class: protocol.TrafficCustomRealtime, Datagrams: []RealtimeDatagram{{Data: []byte{4}, Destination: destination}}},
	} {
		if err := endpointUDP.SendRealtimeBatch(batch); err != nil {
			t.Fatal(err)
		}
	}
	endpointUDP.InvalidateRealtime(2)
	if len(endpointUDP.audioBatches) != 1 || len(endpointUDP.interactiveBatches) != 1 {
		t.Fatalf("realtime lanes after invalidation = audio %d, interactive %d", len(endpointUDP.audioBatches), len(endpointUDP.interactiveBatches))
	}
	if batch := <-endpointUDP.audioBatches; batch.Generation != 0 || batch.Datagrams[0].Data[0] != 2 {
		t.Fatalf("retained audio batch = %#v", batch)
	}
	if batch := <-endpointUDP.interactiveBatches; batch.Generation != 0 || batch.Datagrams[0].Data[0] != 4 {
		t.Fatalf("retained interactive batch = %#v", batch)
	}
	stale := RealtimeBatch{Class: protocol.TrafficAudio, Generation: 1, Datagrams: []RealtimeDatagram{{Data: []byte{5}, Destination: destination}}}
	if err := endpointUDP.SendRealtimeBatch(stale); err == nil {
		t.Fatal("stale local realtime generation was accepted")
	}
	stale.Generation = 2
	if err := endpointUDP.SendRealtimeBatch(stale); err != nil {
		t.Fatalf("current local realtime generation was rejected: %v", err)
	}
}

func TestRealtimeBatchRejectsAggregateBounds(t *testing.T) {
	endpointUDP := New(Options{}, endpointTestRoomTag, testLogger())
	destination := netip.MustParseAddrPort("127.0.0.1:9000")

	tooMany := make([]RealtimeDatagram, MaxRealtimeBatchDatagrams+1)
	for index := range tooMany {
		tooMany[index] = RealtimeDatagram{Data: []byte{1}, Destination: destination}
	}
	if err := endpointUDP.SendRealtimeBatch(RealtimeBatch{Class: protocol.TrafficAudio, Datagrams: tooMany}); err == nil {
		t.Fatal("realtime batch above the datagram count budget was accepted")
	}

	oversizedCount := maxRealtimeBatchBytes/maxDatagramSize + 1
	if oversizedCount > MaxRealtimeBatchDatagrams {
		t.Fatal("test cannot exceed the byte budget within the datagram count budget")
	}
	largeData := make([]byte, maxDatagramSize)
	tooLarge := make([]RealtimeDatagram, oversizedCount)
	for index := range tooLarge {
		tooLarge[index] = RealtimeDatagram{Data: largeData, Destination: destination}
	}
	if err := endpointUDP.SendRealtimeBatch(RealtimeBatch{Class: protocol.TrafficAudio, Datagrams: tooLarge}); err == nil {
		t.Fatal("realtime batch above the aggregate byte budget was accepted")
	}
}

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
	}, endpointTestRoomTag, testLogger())
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
	if !hasCandidate(snapshot.Candidates, CandidateSTUN, client.String()) {
		t.Fatalf("STUN candidate missing: %#v", snapshot.Candidates)
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
	}, endpointTestRoomTag, testLogger())
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
	first := New(Options{ListenAddress: "127.0.0.1:0", STUNServers: []string{}, STUNRefresh: 0}, endpointTestRoomTag, testLogger())
	second := New(Options{ListenAddress: "127.0.0.1:0", STUNServers: []string{}, STUNRefresh: 0}, endpointTestRoomTag, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() { firstDone <- first.Run(ctx) }()
	go func() { secondDone <- second.Run(ctx) }()
	firstAddress := waitForListenAddress(t, first)
	secondAddress := waitForListenAddress(t, second)

	payload := testControlPacket(t, protocol.PacketPing, endpointTestRoomTag)
	if err := first.EnqueueControl(payload, secondAddress); err != nil {
		t.Fatalf("EnqueueControl() error = %v", err)
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
	for _, test := range []struct {
		name    string
		payload []byte
		packets <-chan Datagram
	}{
		{name: "reliable", payload: testReliablePacket(t, endpointTestRoomTag), packets: second.ReliablePackets()},
		{name: "bridge", payload: testBridgePacket(t, endpointTestRoomTag), packets: second.BridgePackets()},
	} {
		if err := first.EnqueueControl(test.payload, secondAddress); err != nil {
			t.Fatalf("send %s packet: %v", test.name, err)
		}
		select {
		case packet := <-test.packets:
			if !bytes.Equal(packet.Data, test.payload) {
				t.Fatalf("%s packet data mismatch", test.name)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for %s packet", test.name)
		}
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
	endpoint := New(Options{ListenAddress: "127.0.0.1:0", STUNServers: []string{}, STUNRefresh: 0}, endpointTestRoomTag, testLogger())
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
	want := testControlPacket(t, protocol.PacketPing, endpointTestRoomTag)
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

func TestClassifyRoomPacket(t *testing.T) {
	endpoint := New(Options{}, endpointTestRoomTag, testLogger())
	otherRoomTag := [16]byte{2}
	invalidGroup := testGroupDatagram(t, protocol.TrafficAudio, endpointTestRoomTag)
	invalidGroup[len(protocol.Magic)+2+len(endpointTestRoomTag)] = 0
	tests := []struct {
		name   string
		packet []byte
		want   packetClass
	}{
		{name: "hello", packet: testHelloPacket(t, endpointTestRoomTag), want: packetControl},
		{name: "ping", packet: testControlPacket(t, protocol.PacketPing, endpointTestRoomTag), want: packetControl},
		{name: "pong", packet: testControlPacket(t, protocol.PacketPong, endpointTestRoomTag), want: packetControl},
		{name: "reliable", packet: testReliablePacket(t, endpointTestRoomTag), want: packetReliable},
		{name: "bridge", packet: testBridgePacket(t, endpointTestRoomTag), want: packetBridge},
		{name: "audio", packet: testGroupDatagram(t, protocol.TrafficAudio, endpointTestRoomTag), want: packetAudio},
		{name: "interactive", packet: testGroupDatagram(t, protocol.TrafficInteractive, endpointTestRoomTag), want: packetInteractive},
		{name: "custom realtime", packet: testGroupDatagram(t, protocol.TrafficCustomRealtime, endpointTestRoomTag), want: packetInteractive},
		{name: "invalid group class", packet: invalidGroup, want: packetDrop},
		{name: "wrong room", packet: testControlPacket(t, protocol.PacketPing, otherRoomTag), want: packetDrop},
		{name: "truncated", packet: testControlPacket(t, protocol.PacketPing, endpointTestRoomTag)[:10], want: packetDrop},
		{name: "opaque", packet: []byte("not a Bork packet"), want: packetDrop},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := endpoint.classifyRoomPacket(test.packet); got != test.want {
				t.Fatalf("classifyRoomPacket() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestRealtimeQueuesDropWholeOldestBatch(t *testing.T) {
	endpoint := New(Options{}, endpointTestRoomTag, testLogger())
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	endpoint.conn = conn
	destination := netip.MustParseAddrPort("127.0.0.1:9000")
	for _, test := range []struct {
		name     string
		class    protocol.TrafficClass
		capacity int
		queue    chan RealtimeBatch
	}{
		{name: "audio", class: protocol.TrafficAudio, capacity: maxAudioBatches, queue: endpoint.audioBatches},
		{name: "interactive", class: protocol.TrafficInteractive, capacity: maxInteractiveBatches, queue: endpoint.interactiveBatches},
	} {
		t.Run(test.name, func(t *testing.T) {
			for index := range test.capacity + 1 {
				batch := RealtimeBatch{Class: test.class, Datagrams: []RealtimeDatagram{
					{Data: []byte{byte(index), 1}, Destination: destination},
					{Data: []byte{byte(index), 2}, Destination: destination},
				}}
				if err := endpoint.SendRealtimeBatch(batch); err != nil {
					t.Fatal(err)
				}
			}
			if len(test.queue) != test.capacity {
				t.Fatalf("queued batches = %d, want %d", len(test.queue), test.capacity)
			}
			oldest := <-test.queue
			if len(oldest.Datagrams) != 2 || oldest.Datagrams[0].Data[0] != 1 || oldest.Datagrams[1].Data[0] != 1 {
				t.Fatalf("oldest retained batch = %#v", oldest)
			}
		})
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

func TestReliableIngressLimiterBoundsEachPath(t *testing.T) {
	limiter := newIngressLimiter(reliableSourceRate, reliableSourceBurst, reliableGlobalRate, reliableGlobalBurst, maxReliableSources)
	now := time.Unix(1000, 0)
	first := netip.MustParseAddrPort("192.0.2.1:9000")
	for range int(reliableSourceBurst) {
		if !limiter.allow(first, now) {
			t.Fatal("reliable limiter rejected a packet within the path burst")
		}
	}
	if limiter.allow(first, now) {
		t.Fatal("reliable limiter accepted a packet above the path burst")
	}
	if !limiter.allow(netip.MustParseAddrPort("192.0.2.1:9001"), now) {
		t.Fatal("reliable limiter coupled different paths behind one IP")
	}
	if !limiter.allow(first, now.Add(time.Second)) {
		t.Fatal("reliable limiter did not replenish path tokens")
	}
}

func TestAudioIPLimiterAllowsForwarderAggregateAndBoundsSourcePorts(t *testing.T) {
	limiter := newIngressLimiter(audioIPRate, audioIPBurst, audioGlobalRate, audioGlobalBurst, maxAudioSources)
	now := time.Unix(1000, 0)
	address := netip.MustParseAddr("192.0.2.1")
	for index := range int(audioIPBurst) {
		source := netip.AddrPortFrom(address, uint16(index+1))
		ipSource := netip.AddrPortFrom(source.Addr(), 0)
		if !limiter.allow(ipSource, now) {
			t.Fatal("audio IP limiter rejected a packet within the IP burst")
		}
	}
	if limiter.allow(netip.AddrPortFrom(address, 0), now) {
		t.Fatal("rotating source ports bypassed the audio IP burst")
	}
	if !limiter.allow(netip.MustParseAddrPort("192.0.2.2:0"), now) {
		t.Fatal("one audio source consumed the global ingress budget")
	}
	if !limiter.allow(netip.AddrPortFrom(address, 0), now.Add(time.Second)) {
		t.Fatal("audio IP limiter did not replenish aggregate tokens")
	}
}

func TestInteractiveIngressLimiterBoundsOneSource(t *testing.T) {
	limiter := newIngressLimiter(interactiveSourceRate, interactiveSourceBurst, interactiveGlobalRate, interactiveGlobalBurst, maxInteractiveSources)
	now := time.Unix(1000, 0)
	source := netip.MustParseAddrPort("192.0.2.1:9000")
	for range int(interactiveSourceBurst) {
		if !limiter.allow(source, now) {
			t.Fatal("interactive limiter rejected a packet within the source burst")
		}
	}
	if limiter.allow(source, now) {
		t.Fatal("interactive limiter accepted a packet above the source burst")
	}
}

func TestQueueWriteDeadlineBoundsWait(t *testing.T) {
	endpoint := New(Options{}, endpointTestRoomTag, testLogger())
	started := time.Now()
	ctx, cancel := context.WithDeadline(context.Background(), started.Add(20*time.Millisecond))
	defer cancel()
	err := endpoint.queueWrite(ctx, endpoint.controlWrites, []byte{1}, netip.MustParseAddrPort("127.0.0.1:9000"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queueWrite() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("queued write ignored deadline for %v", elapsed)
	}
	request := <-endpoint.controlWrites
	if !bytes.Equal(request.data, []byte{1}) {
		t.Fatalf("queued data = %v", request.data)
	}
	if err := writeQueued(nil, request); err != nil {
		t.Fatalf("expired write returned error = %v", err)
	}
	if result := <-request.result; !errors.Is(result, context.DeadlineExceeded) {
		t.Fatalf("expired queued write result = %v", result)
	}
}

func TestWriterExitClosesConcurrentQueueAdmission(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	endpointUDP := New(Options{}, endpointTestRoomTag, testLogger())
	endpointUDP.mu.Lock()
	endpointUDP.conn = conn
	endpointUDP.mu.Unlock()
	destination := conn.LocalAddr().(*net.UDPAddr).AddrPort()

	ctx, cancel := context.WithCancel(context.Background())
	writerExited := make(chan struct{})
	go func() {
		_ = endpointUDP.writeLoop(ctx, conn)
		close(writerExited)
	}()

	var workers sync.WaitGroup
	failures := make(chan string, 32)
	stopProducers := make(chan struct{})
	for range 16 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-stopProducers:
					<-writerExited
					if err := endpointUDP.EnqueueControl([]byte{1}, destination); err == nil {
						failures <- "control enqueue succeeded after writer exit"
					}
					batch := RealtimeBatch{Class: protocol.TrafficAudio, Datagrams: []RealtimeDatagram{{Data: []byte{1}, Destination: destination}}}
					if err := endpointUDP.SendRealtimeBatch(batch); err == nil {
						failures <- "realtime enqueue succeeded after writer exit"
					}
					return
				default:
					_ = endpointUDP.EnqueueControl([]byte{1}, destination)
				}
			}
		}()
	}
	time.Sleep(10 * time.Millisecond)
	close(stopProducers)
	cancel()
	select {
	case <-writerExited:
	case <-time.After(time.Second):
		t.Fatal("writer did not exit")
	}
	workers.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
}

func TestWriteLoopUsesWeightedCycleWithoutStarvingLanes(t *testing.T) {
	receiver, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	sender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()

	endpoint := New(Options{}, endpointTestRoomTag, testLogger())
	destination := receiver.LocalAddr().(*net.UDPAddr).AddrPort()
	deadline := time.Now().Add(5 * time.Second)
	audio := RealtimeBatch{
		Class: protocol.TrafficAudio, Deadline: deadline,
	}
	for range 16 {
		audio.Datagrams = append(audio.Datagrams, RealtimeDatagram{Data: []byte{1}, Destination: destination})
	}
	control := func() queuedWrite {
		return queuedWrite{data: []byte{2}, destination: destination, deadline: deadline, result: make(chan error, 1)}
	}
	for range cap(endpoint.audioBatches) {
		endpoint.audioBatches <- audio
	}
	for range cap(endpoint.controlWrites) {
		endpoint.controlWrites <- control()
	}
	endpoint.interactiveBatches <- RealtimeBatch{
		Class: protocol.TrafficInteractive, Deadline: deadline,
		Datagrams: []RealtimeDatagram{{Data: []byte{3}, Destination: destination}},
	}
	endpoint.backgroundWrites <- queuedWrite{
		data: []byte{4}, destination: destination, deadline: deadline, result: make(chan error, 1),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- endpoint.writeLoop(ctx, sender) }()
	if err := receiver.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 0, 12)
	buffer := make([]byte, 1)
	for range 12 {
		count, _, err := receiver.ReadFromUDPAddrPort(buffer)
		if err != nil {
			t.Fatalf("read scheduled write: %v", err)
		}
		if count != 1 {
			t.Fatalf("scheduled datagram size = %d", count)
		}
		got = append(got, buffer[0])
	}
	want := []byte{1, 1, 1, 1, 1, 1, 1, 1, 2, 2, 3, 4}
	if !bytes.Equal(got, want) {
		t.Fatalf("scheduled writes = %v, want %v", got, want)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("writeLoop() error = %v", err)
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

func testControlPacket(t testing.TB, packetType protocol.PacketType, roomTag [16]byte) []byte {
	t.Helper()
	packet, err := protocol.MarshalControl(packetType, roomTag, [16]byte{1}, 1, 1, testPairwiseCipher(t))
	if err != nil {
		t.Fatal(err)
	}
	return packet
}

func testHelloPacket(t testing.TB, roomTag [16]byte) []byte {
	t.Helper()
	signer := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	packet, err := protocol.MarshalHello(roomTag, [32]byte{}, signer, [16]byte{1}, [32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	return packet
}

func testReliablePacket(t testing.TB, roomTag [16]byte) []byte {
	t.Helper()
	packet, err := protocol.MarshalReliable(roomTag, [16]byte{1}, 1, protocol.ReliablePacket{
		Channel:          1,
		FragmentSequence: 1,
		MessageSequence:  1,
		FragmentCount:    1,
		Payload:          []byte{1},
	}, testPairwiseCipher(t))
	if err != nil {
		t.Fatal(err)
	}
	return packet
}

func testBridgePacket(t testing.TB, roomTag [16]byte) []byte {
	t.Helper()
	packet, err := protocol.MarshalBridge(roomTag, [16]byte{1}, 1, [32]byte{2}, [32]byte{3}, false, testControlPacket(t, protocol.PacketPing, roomTag), testPairwiseCipher(t))
	if err != nil {
		t.Fatal(err)
	}
	return packet
}

func testGroupDatagram(t testing.TB, class protocol.TrafficClass, roomTag [16]byte) []byte {
	t.Helper()
	publicKey, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var senderID [32]byte
	copy(senderID[:], publicKey)
	protector := protocol.NewGroupDatagramCipher([32]byte{})
	packet, err := protocol.MarshalGroupDatagram(roomTag, protocol.GroupDatagramHeader{
		Class:    class,
		SenderID: senderID,
		StreamID: [16]byte{1},
		Sequence: 1,
	}, 1, []byte{1}, protector, signer)
	if err != nil {
		t.Fatal(err)
	}
	return packet
}

func testPairwiseCipher(t testing.TB) cipher.AEAD {
	t.Helper()
	protector, err := chacha20poly1305.New(make([]byte, chacha20poly1305.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return protector
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

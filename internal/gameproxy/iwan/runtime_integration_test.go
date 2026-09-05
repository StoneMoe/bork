package iwan

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestDefaultRuntimeTimings_matchProtocolContract(t *testing.T) {
	timings := defaultRuntimeTimings()
	if timings.openRetry != 2*time.Second || timings.authTimeout != 6*time.Second ||
		timings.echoInterval != 2*time.Second || timings.liveness != 15*time.Second ||
		timings.restartDelay != time.Second {
		t.Fatalf("default timings = %#v", timings)
	}
}

func TestGeneration_retriesDefaultPlaintextOpen_thenNegotiatesMTU(t *testing.T) {
	server := newFakeReferenceServer(t)
	supervisor, err := newSupervisor(testOptions(t, server), testTimings())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(supervisor.Stop)

	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := server.next(t)
	second := server.next(t)
	for _, datagram := range []receivedDatagram{first, second} {
		request, parseErr := ParseOpen(datagram.packet)
		if parseErr != nil || request.Encrypt || request.MTU != DefaultMTU {
			t.Fatalf("OPEN = (%#v, %v)", request, parseErr)
		}
	}
	if _, err := supervisor.OpenUDP(); !errors.Is(err, ErrNotReady) {
		t.Fatalf("OpenUDP before ACK = %v, want ErrNotReady", err)
	}
	if _, err := supervisor.DialTCP(context.Background(), netip.MustParseAddrPort("10.20.30.50:80")); !errors.Is(err, ErrNotReady) {
		t.Fatalf("DialTCP before ACK = %v, want ErrNotReady", err)
	}
	ack := buildTestACK(ackSpec{token: Token{1, 2}, session: SessionID{3, 4, 5, 6}, mtu: 1300})
	server.send(t, second.peer, ack)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := supervisor.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	status := supervisor.Status()
	if status.State != StateReady || status.MTU != 1300 || status.Generation != 1 {
		t.Fatalf("status = %#v", status)
	}
}

func TestGeneration_authRejectIsTerminal(t *testing.T) {
	server := newFakeReferenceServer(t)
	supervisor, err := newSupervisor(testOptions(t, server), testTimings())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(supervisor.Stop)
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	open := server.next(t)
	server.send(t, open.peer, BuildOpenReject())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := supervisor.WaitReady(ctx); !errors.Is(err, ErrAuthRejected) {
		t.Fatalf("WaitReady error = %v, want ErrAuthRejected", err)
	}
	server.expectNone(t, 2*testTimings().restartDelay)
}

func TestGeneration_ignoresMalformedACKUntilValidACK(t *testing.T) {
	server := newFakeReferenceServer(t)
	supervisor, err := newSupervisor(testOptions(t, server), testTimings())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(supervisor.Stop)
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	open := server.next(t)
	ack := buildTestACK(ackSpec{token: Token{1, 2}, session: SessionID{3, 4, 5, 6}, mtu: 1400})
	malformed := append([]byte(nil), ack...)
	copy(malformed[30:34], []byte{0, 0, 0, 0})
	server.send(t, open.peer, malformed)
	server.send(t, open.peer, ack)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := supervisor.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	if status := supervisor.Status(); status.Generation != 1 || status.State != StateReady {
		t.Fatalf("status = %#v, want first generation ready", status)
	}
}

func TestGeneration_ignoresPeerMTUBelowIPv4Minimum(t *testing.T) {
	server := newFakeReferenceServer(t)
	supervisor, err := newSupervisor(testOptions(t, server), testTimings())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(supervisor.Stop)
	startAndACK(t, supervisor, handshakeSpec{server: server, ack: ackSpec{
		token: Token{1, 2}, session: SessionID{3, 4, 5, 6}, mtu: 60,
	}})
	if status := supervisor.Status(); status.MTU != DefaultMTU {
		t.Fatalf("effective MTU = %d, want configured %d", status.MTU, DefaultMTU)
	}
}

func TestGeneration_rawIPv4RoundTripUsesPlaintextDataAndFragments(t *testing.T) {
	server := newFakeReferenceServer(t)
	supervisor, err := newSupervisor(testOptions(t, server), testTimings())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(supervisor.Stop)
	_, session := startAndACK(t, supervisor, handshakeSpec{server: server, ack: ackSpec{
		token: Token{1, 2}, session: SessionID{3, 4, 5, 6}, mtu: 1400,
	}})
	packetConn, err := supervisor.OpenUDP()
	if err != nil {
		t.Fatal(err)
	}
	defer packetConn.Close()

	if _, err := packetConn.WriteTo([]byte("outbound"), &net.UDPAddr{IP: net.IPv4(10, 20, 30, 50), Port: 9000}); err != nil {
		t.Fatal(err)
	}
	outbound := server.next(t)
	for PacketType(outbound.packet[0]) == TypeEchoRequest {
		outbound = server.next(t)
	}
	if PacketType(outbound.packet[0]) != TypeData {
		t.Fatalf("outbound type = %#x", outbound.packet[0])
	}
	raw, err := session.ParseData(outbound.packet)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 28 || raw[0]&0x0f != 5 || raw[9] != 17 {
		t.Fatalf("outbound packet is not minimal-header IPv4 UDP: %x", raw)
	}
	if checksum := internetChecksum(raw[:20]); checksum != 0 {
		t.Fatalf("outbound IPv4 checksum residual = %#x, want 0", checksum)
	}
	pseudoHeader := make([]byte, 12+len(raw)-20)
	copy(pseudoHeader[:8], raw[12:20])
	pseudoHeader[9] = raw[9]
	copy(pseudoHeader[10:12], raw[24:26])
	copy(pseudoHeader[12:], raw[20:])
	if checksum := internetChecksum(pseudoHeader); checksum != 0 {
		t.Fatalf("outbound UDP checksum residual = %#x, want 0", checksum)
	}
	clientPort := binary.BigEndian.Uint16(raw[20:22])
	if clientPort == 0 {
		t.Fatal("outbound UDP source port is zero")
	}
	outboundDeadline := time.Now().Add(time.Second)
	for {
		outboundStats := supervisor.Status().DataPath
		if outboundStats.OutboundNodeDatagrams == 1 {
			if outboundStats.OutboundStackPackets != 1 || outboundStats.OutboundStackBytes != uint64(len(raw)) ||
				outboundStats.OutboundUDPPackets != 1 || outboundStats.OutboundNodeBytes != uint64(len(outbound.packet)) {
				t.Fatalf("outbound data path stats = %#v", outboundStats)
			}
			break
		}
		if time.Now().After(outboundDeadline) {
			t.Fatalf("outbound data path stats = %#v", outboundStats)
		}
		time.Sleep(time.Millisecond)
	}
	reply := ipv4UDP(udpPacketSpec{
		source: netip.MustParseAddr("10.20.30.50"), destination: session.Address,
		sourcePort: 9000, destinationPort: clientPort, payload: []byte("inbound"),
	})
	encoded := append([]byte(nil), reply...)
	middle := len(encoded) / 2
	firstFragment := fragmentPacket(session, 44, 0, false, encoded[:middle])
	secondFragment := fragmentPacket(session, 44, middle, true, encoded[middle:])
	server.send(t, outbound.peer, firstFragment)
	server.send(t, outbound.peer, secondFragment)
	if err := packetConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 64)
	read, _, err := packetConn.ReadFrom(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if string(buffer[:read]) != "inbound" {
		t.Fatalf("reply = %q", buffer[:read])
	}
	deadline := time.Now().Add(time.Second)
	for {
		inboundStats := supervisor.Status().DataPath
		if inboundStats.InboundStackPackets == 1 {
			if inboundStats.InboundNodeDatagrams != 2 ||
				inboundStats.InboundNodeBytes != uint64(len(firstFragment)+len(secondFragment)) ||
				inboundStats.InboundFragmentDatagrams != 2 ||
				inboundStats.InboundFragmentBytes != uint64(len(firstFragment)+len(secondFragment)) ||
				inboundStats.InboundFragmentCompletedPackets != 1 ||
				inboundStats.InboundStackBytes != uint64(len(reply)) || inboundStats.InboundUDPPackets != 1 {
				t.Fatalf("inbound data path stats = %#v", inboundStats)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("inbound data path stats were not published: %#v", inboundStats)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestGeneration_respondsToEchoRequestAndSendsPeriodicEcho(t *testing.T) {
	server := newFakeReferenceServer(t)
	supervisor, err := newSupervisor(testOptions(t, server), testTimings())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(supervisor.Stop)
	open, session := startAndACK(t, supervisor, handshakeSpec{server: server, ack: ackSpec{
		token: Token{1, 2}, session: SessionID{3, 4, 5, 6}, mtu: 1400,
	}})

	periodic := server.next(t)
	if control, parseErr := ParseControl(periodic.packet, session); parseErr != nil || control.Type != TypeEchoRequest {
		t.Fatalf("periodic ECHO = (%#v, %v)", control, parseErr)
	}
	request := BuildEchoRequest(session, Echo{Timestamp: time.Unix(10, 0)})
	server.send(t, open.peer, request)
	response := server.next(t)
	if control, parseErr := ParseControl(response.packet, session); parseErr != nil || control.Type != TypeEchoResponse {
		t.Fatalf("ECHO response = (%#v, %v)", control, parseErr)
	}
}

func TestGeneration_echoRequestsCarryTrackedRTT(t *testing.T) {
	session := goldenSession(t)
	current := generation{session: session, echo: Echo{MinimumDelay: ^uint32(0)}}
	now := time.Unix(100, 500_000)
	current.recordEchoResponse(now.Add(-12*time.Millisecond), now)
	current.recordEchoResponse(now.Add(-20*time.Millisecond), now)
	current.recordEchoResponse(now.Add(-5*time.Millisecond), now)
	control, err := ParseControl(current.buildEchoRequest(now), session)
	if err != nil {
		t.Fatal(err)
	}
	if control.Echo.CurrentDelay != 5_000 || control.Echo.MinimumDelay != 5_000 || control.Echo.MaximumDelay != 20_000 {
		t.Fatalf("tracked ECHO = %#v", control.Echo)
	}
}

func TestGeneration_peerCloseTerminatesAndReplacesGeneration(t *testing.T) {
	server := newFakeReferenceServer(t)
	supervisor, err := newSupervisor(testOptions(t, server), testTimings())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(supervisor.Stop)
	open, session := startAndACK(t, supervisor, handshakeSpec{server: server, ack: ackSpec{
		token: Token{1, 2}, session: SessionID{3, 4, 5, 6}, mtu: 1400,
	}})
	server.send(t, open.peer, BuildClose(session))
	for {
		datagram := server.next(t)
		if PacketType(datagram.packet[0]) == TypeOpen {
			if supervisor.Status().Generation != 2 {
				t.Fatalf("replacement status = %#v", supervisor.Status())
			}
			return
		}
	}
}

func TestGeneration_rejectsBadSessionIPv6AndUnknownWithoutLiveness(t *testing.T) {
	server := newFakeReferenceServer(t)
	supervisor, err := newSupervisor(testOptions(t, server), testTimings())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(supervisor.Stop)
	open, session := startAndACK(t, supervisor, handshakeSpec{server: server, ack: ackSpec{
		token: Token{1, 2}, session: SessionID{3, 4, 5, 6}, mtu: 1400,
	}})
	valid, err := session.BuildData([]byte{0x60, 0, 0, 0, 0, 0, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	badSession := append([]byte(nil), valid...)
	badSession[2] ^= 1
	unknown := append([]byte(nil), valid...)
	unknown[0] = 0x7f
	for _, packet := range [][]byte{badSession, valid, unknown} {
		server.send(t, open.peer, packet)
	}

	for {
		datagram := server.next(t)
		if PacketType(datagram.packet[0]) == TypeOpen {
			if supervisor.Status().Generation < 2 {
				t.Fatalf("replacement OPEN with status %#v", supervisor.Status())
			}
			return
		}
	}
}

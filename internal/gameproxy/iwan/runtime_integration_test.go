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

func TestGeneration_retriesForcedXOROpen_thenNegotiatesMTU(t *testing.T) {
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
		if parseErr != nil || !request.XOR || request.MTU != DefaultMTU {
			t.Fatalf("OPEN = (%#v, %v)", request, parseErr)
		}
	}
	if _, err := supervisor.OpenUDP(); !errors.Is(err, ErrNotReady) {
		t.Fatalf("OpenUDP before ACK = %v, want ErrNotReady", err)
	}
	if _, err := supervisor.DialTCP(context.Background(), netip.MustParseAddrPort("10.20.30.50:80")); !errors.Is(err, ErrNotReady) {
		t.Fatalf("DialTCP before ACK = %v, want ErrNotReady", err)
	}
	ack := buildTestACK(ackSpec{token: Token{1, 2}, session: SessionID{3, 4, 5, 6}, mtu: 1300, xor: true})
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

func TestGeneration_authRejectAndDowngradeAreTerminal(t *testing.T) {
	for _, test := range []struct {
		name     string
		response func() []byte
		want     error
	}{
		{name: "rejection", response: func() []byte { return BuildOpenReject() }, want: ErrAuthRejected},
		{name: "downgrade", response: func() []byte {
			return buildTestACK(ackSpec{token: Token{1, 2}, session: SessionID{3, 4, 5, 6}, mtu: 1400})
		}, want: ErrProtocolDowngrade},
		{name: "invalid configuration", response: func() []byte {
			packet := buildTestACK(ackSpec{token: Token{1, 2}, session: SessionID{3, 4, 5, 6}, mtu: 1400, xor: true})
			copy(packet[30:34], []byte{0, 0, 0, 0})
			return packet
		}, want: ErrProtocolConfiguration},
	} {
		t.Run(test.name, func(t *testing.T) {
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
			server.send(t, open.peer, test.response())
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := supervisor.WaitReady(ctx); !errors.Is(err, test.want) {
				t.Fatalf("WaitReady error = %v, want %v", err, test.want)
			}
			server.expectNone(t, 2*testTimings().restartDelay)
		})
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

func TestGeneration_rawIPv4RoundTripUsesEncryptedDataAndFragments(t *testing.T) {
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
	if PacketType(outbound.packet[0]) != TypeDataXOR {
		t.Fatalf("outbound type = %#x", outbound.packet[0])
	}
	raw, err := session.ParseData(outbound.packet)
	if err != nil {
		t.Fatal(err)
	}
	clientPort := binary.BigEndian.Uint16(raw[20:22])
	reply := ipv4UDP(udpPacketSpec{
		source: netip.MustParseAddr("10.20.30.50"), destination: session.Address,
		sourcePort: 9000, destinationPort: clientPort, payload: []byte("inbound"),
	})
	encoded := append([]byte(nil), reply...)
	xorBytes(session.xorKey, encoded)
	middle := len(encoded) / 2
	server.send(t, outbound.peer, fragmentPacket(session, 44, 0, false, encoded[:middle]))
	server.send(t, outbound.peer, fragmentPacket(session, 44, middle, true, encoded[middle:]))
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

func TestGeneration_rejectsBadSessionPlaintextIPv6AndUnknownWithoutLiveness(t *testing.T) {
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
	plaintext := append([]byte(nil), valid...)
	plaintext[0] = byte(TypeData)
	unknown := append([]byte(nil), valid...)
	unknown[0] = 0x7f
	for _, packet := range [][]byte{badSession, plaintext, valid, unknown} {
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

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

type receivedDatagram struct {
	packet []byte
	peer   *net.UDPAddr
}

type fakeReferenceServer struct {
	connection *net.UDPConn
	received   chan receivedDatagram
}

type ackSpec struct {
	token   Token
	session SessionID
	mtu     uint16
	xor     bool
}

type handshakeSpec struct {
	server *fakeReferenceServer
	ack    ackSpec
}

type udpPacketSpec struct {
	source          netip.Addr
	destination     netip.Addr
	sourcePort      uint16
	destinationPort uint16
	payload         []byte
}

func newFakeReferenceServer(t *testing.T) *fakeReferenceServer {
	t.Helper()
	connection, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen fake iwan server: %v", err)
	}
	server := &fakeReferenceServer{connection: connection, received: make(chan receivedDatagram, 64)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close fake iwan server: %v", err)
		}
	})
	go server.readLoop(ctx)
	return server
}

func (server *fakeReferenceServer) readLoop(ctx context.Context) {
	buffer := make([]byte, fragmentOutputSize+fragmentHeaderSize)
	for {
		read, peer, err := server.connection.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		datagram := receivedDatagram{packet: append([]byte(nil), buffer[:read]...), peer: peer}
		select {
		case server.received <- datagram:
		case <-ctx.Done():
			return
		}
	}
}

func (server *fakeReferenceServer) next(t *testing.T) receivedDatagram {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	select {
	case datagram := <-server.received:
		return datagram
	case <-ctx.Done():
		t.Fatal("timed out waiting for iwan datagram")
		return receivedDatagram{}
	}
}

func (server *fakeReferenceServer) expectNone(t *testing.T, duration time.Duration) {
	t.Helper()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case datagram := <-server.received:
		t.Fatalf("unexpected iwan datagram: %x", datagram.packet)
	case <-timer.C:
	}
}

func (server *fakeReferenceServer) drain() {
	for {
		select {
		case <-server.received:
		default:
			return
		}
	}
}

func (server *fakeReferenceServer) send(t *testing.T, peer *net.UDPAddr, packet []byte) {
	t.Helper()
	if _, err := server.connection.WriteToUDP(packet, peer); err != nil {
		t.Fatalf("send fake iwan datagram: %v", err)
	}
}

func (server *fakeReferenceServer) port(t *testing.T) uint16 {
	t.Helper()
	address, ok := server.connection.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatalf("fake server address type = %T", server.connection.LocalAddr())
	}
	return uint16(address.Port)
}

func testOptions(t *testing.T, server *fakeReferenceServer) Options {
	t.Helper()
	return Options{
		Node: Node{
			Server:   "localhost",
			Port:     server.port(t),
			Username: "myuser",
			Password: "mypassword",
			MTU:      DefaultMTU,
		},
		QueueSize: 64,
	}
}

func testTimings() runtimeTimings {
	return runtimeTimings{
		openRetry:    20 * time.Millisecond,
		authTimeout:  100 * time.Millisecond,
		echoInterval: 25 * time.Millisecond,
		liveness:     120 * time.Millisecond,
		restartDelay: 20 * time.Millisecond,
	}
}

func buildTestACK(spec ackSpec) []byte {
	packet := make([]byte, signedHeaderSize)
	writeHeader(packet, wireHeader{typ: TypeOpenACK, flags: 1, token: spec.token, session: spec.session})
	signControl(packet)
	packet = appendTLV(packet, 3, byte(spec.mtu>>8), byte(spec.mtu))
	packet = appendTLV(packet, 4, 10, 20, 30, 40)
	if spec.xor {
		packet = appendTLV(packet, 8, 1)
	}
	return packet
}

func startAndACK(t *testing.T, supervisor *Supervisor, spec handshakeSpec) (receivedDatagram, Session) {
	t.Helper()
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatalf("start supervisor: %v", err)
	}
	open := spec.server.next(t)
	credentials := testCredentials(t)
	spec.ack.xor = true
	ack := buildTestACK(spec.ack)
	session, err := ParseOpenACK(ack, credentials)
	if err != nil {
		t.Fatalf("parse test OPENACK: %v", err)
	}
	spec.server.send(t, open.peer, ack)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := supervisor.WaitReady(ctx); err != nil {
		t.Fatalf("wait ready: %v", err)
	}
	return open, session
}

func testCredentials(t testing.TB) Credentials {
	t.Helper()
	credentials, err := NewCredentials("myuser", "mypassword")
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	return credentials
}

func ipv4UDP(spec udpPacketSpec) []byte {
	packet := make([]byte, 28+len(spec.payload))
	packet[0] = 0x45
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	packet[8] = 64
	packet[9] = 17
	sourceBytes := spec.source.As4()
	destinationBytes := spec.destination.As4()
	copy(packet[12:16], sourceBytes[:])
	copy(packet[16:20], destinationBytes[:])
	binary.BigEndian.PutUint16(packet[10:12], ipv4Checksum(packet[:20]))
	binary.BigEndian.PutUint16(packet[20:22], spec.sourcePort)
	binary.BigEndian.PutUint16(packet[22:24], spec.destinationPort)
	binary.BigEndian.PutUint16(packet[24:26], uint16(8+len(spec.payload)))
	copy(packet[28:], spec.payload)
	return packet
}

func ipv4Checksum(header []byte) uint16 {
	var sum uint32
	for index := 0; index < len(header); index += 2 {
		sum += uint32(binary.BigEndian.Uint16(header[index : index+2]))
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

package intercept

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

type packetWrite struct {
	payload     []byte
	destination netip.AddrPort
}

type packetRead struct {
	payload []byte
	source  netip.AddrPort
}

type fakePacketConn struct {
	reads        chan packetRead
	readObserved chan struct{}
	writes       chan packetWrite
	writeStarted chan []byte
	writeGate    chan struct{}
	writeErr     error
	closed       chan struct{}
	closeOnce    sync.Once
}

func newFakePacketConn() *fakePacketConn {
	return &fakePacketConn{
		reads: make(chan packetRead, 4), writes: make(chan packetWrite, 4),
		readObserved: make(chan struct{}, 4), writeStarted: make(chan []byte, 4),
		closed: make(chan struct{}),
	}
}

func (connection *fakePacketConn) ReadFrom(buffer []byte) (int, net.Addr, error) {
	select {
	case packet := <-connection.reads:
		count := copy(buffer, packet.payload)
		connection.readObserved <- struct{}{}
		return count, net.UDPAddrFromAddrPort(packet.source), nil
	case <-connection.closed:
		return 0, nil, net.ErrClosed
	}
}

func (connection *fakePacketConn) WriteTo(buffer []byte, destination net.Addr) (int, error) {
	address := destination.(*net.UDPAddr).AddrPort()
	connection.writeStarted <- buffer
	if connection.writeGate != nil {
		select {
		case <-connection.writeGate:
		case <-connection.closed:
			return 0, net.ErrClosed
		}
	}
	if connection.writeErr != nil {
		return 0, connection.writeErr
	}
	payload := append([]byte(nil), buffer...)
	connection.writes <- packetWrite{payload: payload, destination: address}
	return len(buffer), nil
}

func (connection *fakePacketConn) Close() error {
	connection.closeOnce.Do(func() { close(connection.closed) })
	return nil
}
func (*fakePacketConn) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (*fakePacketConn) SetDeadline(time.Time) error      { return nil }
func (*fakePacketConn) SetReadDeadline(time.Time) error  { return nil }
func (*fakePacketConn) SetWriteDeadline(time.Time) error { return nil }

type fakeUDPEndpoint struct {
	metadata     Metadata
	reads        chan Datagram
	writes       chan Datagram
	writeStarted chan Datagram
	writeGate    chan struct{}
	reset        chan error
	closed       chan struct{}
	readObserved chan struct{}
	closeOnce    sync.Once
}

func newFakeUDPEndpoint(metadata Metadata) *fakeUDPEndpoint {
	return &fakeUDPEndpoint{
		metadata: metadata, reads: make(chan Datagram, 4), writes: make(chan Datagram, 4),
		writeStarted: make(chan Datagram, 4), reset: make(chan error, 2),
		closed: make(chan struct{}), readObserved: make(chan struct{}, 4),
	}
}

func (endpoint *fakeUDPEndpoint) Metadata() Metadata { return endpoint.metadata }
func (endpoint *fakeUDPEndpoint) ReadDatagram(ctx context.Context) (Datagram, error) {
	select {
	case datagram := <-endpoint.reads:
		endpoint.readObserved <- struct{}{}
		return datagram, nil
	case <-endpoint.closed:
		return Datagram{}, net.ErrClosed
	case <-ctx.Done():
		return Datagram{}, context.Cause(ctx)
	}
}
func (endpoint *fakeUDPEndpoint) WriteDatagram(ctx context.Context, datagram Datagram) error {
	endpoint.writeStarted <- datagram
	if endpoint.writeGate != nil {
		select {
		case <-endpoint.writeGate:
		case <-endpoint.closed:
			return net.ErrClosed
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	select {
	case endpoint.writes <- datagram:
		return nil
	case <-endpoint.closed:
		return net.ErrClosed
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}
func (endpoint *fakeUDPEndpoint) Reset(cause error) error {
	select {
	case endpoint.reset <- cause:
	default:
	}
	return nil
}
func (endpoint *fakeUDPEndpoint) Close() error {
	endpoint.closeOnce.Do(func() { close(endpoint.closed) })
	return nil
}

func waitPacketWrite(t *testing.T, result <-chan packetWrite) packetWrite {
	t.Helper()
	select {
	case packet := <-result:
		return packet
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for packet write")
		return packetWrite{}
	}
}

func waitDatagram(t *testing.T, result <-chan Datagram) Datagram {
	t.Helper()
	select {
	case datagram := <-result:
		return datagram
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for native datagram")
		return Datagram{}
	}
}

func waitBytes(t *testing.T, result <-chan []byte) []byte {
	t.Helper()
	select {
	case buffer := <-result:
		return buffer
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for bytes")
		return nil
	}
}

func waitEvent(t *testing.T, result <-chan struct{}) {
	t.Helper()
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

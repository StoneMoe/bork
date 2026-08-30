package intercept

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

func TestRelay_TCP_rewrites_DNS_and_relays_both_directions(t *testing.T) {
	nativeRelay, nativeApp := net.Pipe()
	stackRelay, stackServer := net.Pipe()
	flow := newPipeTCP(testMetadata(6, 21, 53), nativeRelay)
	stack := &halfCloseConn{Conn: stackRelay}
	dialer := &fakeDialer{tcpConn: stack}
	relay := newTestRelay(t, fakeRules{selected: true}, dialer)
	relay.SetState(GenerationState{Generation: 6, Ready: true})

	if err := relay.TCP(context.Background(), flow); err != nil {
		t.Fatal(err)
	}
	writeAndRead(t, pipeExchange{source: nativeApp, destination: stackServer, payload: []byte("native payload")})
	writeAndRead(t, pipeExchange{source: stackServer, destination: nativeApp, payload: []byte("stack payload")})
	if err := nativeApp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stackServer.Close(); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, flow.closed)

	if target := dialer.lastTCPTarget(); target != netip.MustParseAddrPort("1.1.1.1:53") {
		t.Fatalf("DialTCP target = %v, want 1.1.1.1:53", target)
	}
	if flow.halfCloseCounts() != (closeCounts{read: 1, write: 1}) {
		t.Fatalf("native half closes = %+v, want one read and one write", flow.halfCloseCounts())
	}
	if stack.halfCloseCounts() != (closeCounts{read: 1, write: 1}) {
		t.Fatalf("stack half closes = %+v, want one read and one write", stack.halfCloseCounts())
	}
}

func TestRelay_TCP_resets_flow_when_dial_fails(t *testing.T) {
	dialFailure := errors.New("dial unavailable")
	dialer := &fakeDialer{tcpErr: dialFailure}
	relay := newTestRelay(t, fakeRules{selected: true}, dialer)
	relay.SetState(GenerationState{Generation: 7, Ready: true})
	flow := newPipeTCP(testMetadata(7, 22, 443), &eofConn{})

	if err := relay.TCP(context.Background(), flow); err != nil {
		t.Fatal(err)
	}
	resetErr := waitError(t, flow.reset)

	if !errors.Is(resetErr, ErrDial) || !errors.Is(resetErr, dialFailure) {
		t.Fatalf("reset error = %v, want ErrDial wrapping dial failure", resetErr)
	}
}

func TestRelay_SetState_resets_active_TCP_flow(t *testing.T) {
	nativeRelay, nativeApp := net.Pipe()
	stackRelay, stackServer := net.Pipe()
	flow := newPipeTCP(testMetadata(9, 23, 443), nativeRelay)
	dialer := &fakeDialer{tcpConn: stackRelay, tcpCalled: make(chan struct{})}
	relay := newTestRelay(t, fakeRules{selected: true}, dialer)
	relay.SetState(GenerationState{Generation: 9, Ready: true})
	if err := relay.TCP(context.Background(), flow); err != nil {
		t.Fatal(err)
	}
	waitDial(t, dialer)

	relay.SetState(GenerationState{Generation: 10})
	resetErr := waitError(t, flow.reset)

	if !errors.Is(resetErr, ErrNotReady) {
		t.Fatalf("reset error = %v, want ErrNotReady", resetErr)
	}
	nativeApp.Close()
	stackServer.Close()
}

type closeCounts struct {
	read  int
	write int
}

type halfCloseConn struct {
	net.Conn
	mu     sync.Mutex
	counts closeCounts
}

func (connection *halfCloseConn) CloseRead() error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	connection.counts.read++
	return nil
}

func (connection *halfCloseConn) CloseWrite() error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	connection.counts.write++
	return nil
}

func (connection *halfCloseConn) halfCloseCounts() closeCounts {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return connection.counts
}

type pipeTCP struct {
	*halfCloseConn
	metadata Metadata
	reset    chan error
	closed   chan struct{}
	once     sync.Once
}

func newPipeTCP(metadata Metadata, connection net.Conn) *pipeTCP {
	return &pipeTCP{
		halfCloseConn: &halfCloseConn{Conn: connection}, metadata: metadata,
		reset: make(chan error, 1), closed: make(chan struct{}),
	}
}

func (flow *pipeTCP) Metadata() Metadata { return flow.metadata }
func (flow *pipeTCP) Reset(cause error) error {
	select {
	case flow.reset <- cause:
	default:
	}
	return nil
}
func (flow *pipeTCP) Close() error {
	err := flow.halfCloseConn.Close()
	flow.once.Do(func() { close(flow.closed) })
	return err
}

type eofConn struct{}

func (*eofConn) Read([]byte) (int, error)         { return 0, net.ErrClosed }
func (*eofConn) Write([]byte) (int, error)        { return 0, net.ErrClosed }
func (*eofConn) Close() error                     { return nil }
func (*eofConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (*eofConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (*eofConn) SetDeadline(time.Time) error      { return nil }
func (*eofConn) SetReadDeadline(time.Time) error  { return nil }
func (*eofConn) SetWriteDeadline(time.Time) error { return nil }

type pipeExchange struct {
	source      net.Conn
	destination net.Conn
	payload     []byte
}

func writeAndRead(t *testing.T, exchange pipeExchange) {
	t.Helper()
	written := make(chan error, 1)
	go func() {
		_, err := exchange.source.Write(exchange.payload)
		written <- err
	}()
	buffer := make([]byte, len(exchange.payload))
	if _, err := io.ReadFull(exchange.destination, buffer); err != nil {
		t.Fatal(err)
	}
	if string(buffer) != string(exchange.payload) {
		t.Fatalf("relayed payload = %q, want %q", buffer, exchange.payload)
	}
	if err := waitError(t, written); err != nil {
		t.Fatal(err)
	}
}

func waitDial(t *testing.T, dialer *fakeDialer) {
	t.Helper()
	if dialer.tcpCalled == nil {
		t.Fatal("test dialer has no call signal")
	}
	waitClosed(t, dialer.tcpCalled)
}

func waitError(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for error")
		return nil
	}
}

func waitClosed(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for close")
	}
}

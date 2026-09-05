package netfilter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"bork/internal/gameproxy/intercept"
)

func TestBridge_redirects_TCP_listener_connection_with_original_metadata(t *testing.T) {
	backend := &tcpTestBackend{}
	callbacks := &tcpTestCallbacks{
		state:    intercept.GenerationState{Generation: 17, Ready: true},
		accepted: make(chan tcpCallbackRecord, 1),
	}
	bridge := startTCPTestBridge(t, backend, callbacks)
	t.Cleanup(func() { _ = bridge.Close() })
	queries := make(chan [2]netip.AddrPort, 1)
	bridge.lookupTCPOwner = func(local, remote netip.AddrPort) (intercept.ProcessID, error) {
		queries <- [2]netip.AddrPort{local, remote}
		return 41, nil
	}
	sourcePort := reserveTCPPort(t)
	event := nativeTCPConnectRequestEvent{
		ID: 71, PID: 41, ExecutablePath: `c:\games\game.exe`,
		Local:  netip.AddrPortFrom(netip.MustParseAddr("10.0.0.2"), sourcePort),
		Remote: netip.MustParseAddrPort("8.8.8.8:443"),
	}

	redirectPort := bridge.tcpConnectRequest(event)
	if redirectPort == 0 {
		t.Fatal("tcpConnectRequest() returned no redirect port")
	}
	roguePort := reserveTCPPort(t)
	rogueDialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(roguePort)}}
	rogue, err := rogueDialer.DialContext(context.Background(), "tcp4", net.JoinHostPort("127.0.0.1", portString(redirectPort)))
	if err != nil {
		t.Fatal(err)
	}
	_ = rogue.Close()
	select {
	case <-callbacks.accepted:
		t.Fatal("unexpected source port consumed TCP redirect mapping")
	case <-time.After(20 * time.Millisecond):
	}

	dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(sourcePort)}}
	client, err := dialer.DialContext(context.Background(), "tcp4", net.JoinHostPort("127.0.0.1", portString(redirectPort)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	var record tcpCallbackRecord
	select {
	case record = <-callbacks.accepted:
	case <-time.After(time.Second):
		t.Fatal("redirected TCP connection was not admitted")
	}
	want := intercept.Metadata{
		Generation: 17, NativeID: 71, ProcessID: 41,
		ExecutablePath: event.ExecutablePath, NativeRuleMatched: true,
		OriginalLocal: event.Local, OriginalRemote: event.Remote,
	}
	if got := record.flow.Metadata(); got != want {
		t.Fatalf("flow metadata = %+v, want %+v", got, want)
	}
	query := <-queries
	wantQuery := [2]netip.AddrPort{
		client.LocalAddr().(*net.TCPAddr).AddrPort(), client.RemoteAddr().(*net.TCPAddr).AddrPort(),
	}
	if query != wantQuery || query[0] == event.Local || query[1] == event.Remote {
		t.Fatalf("ownership query = %v, want actual client tuple %v, not original tuple", query, wantQuery)
	}

	if _, err := client.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	request := make([]byte, len("request"))
	if _, err := io.ReadFull(record.flow, request); err != nil || string(request) != "request" {
		t.Fatalf("redirected request = %q, %v", request, err)
	}
	if _, err := record.flow.Write([]byte("response")); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, len("response"))
	if _, err := io.ReadFull(client, response); err != nil || string(response) != "response" {
		t.Fatalf("redirected response = %q, %v", response, err)
	}
}

func TestBridge_TCP_redirect_rejects_unverified_owner_with_matching_source_port(t *testing.T) {
	lookupErr := errors.New("TCP owner table unavailable")
	for _, test := range []struct {
		name string
		pid  intercept.ProcessID
		err  error
	}{
		{name: "different process", pid: 42},
		{name: "missing owner"},
		{name: "lookup failure", pid: 41, err: lookupErr},
	} {
		t.Run(test.name, func(t *testing.T) {
			callbacks := &tcpTestCallbacks{
				state:    intercept.GenerationState{Generation: 7, Ready: true},
				accepted: make(chan tcpCallbackRecord, 1), endpointErrors: make(chan error, 1),
			}
			bridge := startTCPTestBridge(t, &tcpTestBackend{}, callbacks)
			t.Cleanup(func() { _ = bridge.Close() })
			bridge.lookupTCPOwner = func(netip.AddrPort, netip.AddrPort) (intercept.ProcessID, error) {
				return test.pid, test.err
			}
			sourcePort := reserveTCPPort(t)
			event := validTCPConnectRequest(93, sourcePort)
			redirectPort := bridge.tcpConnectRequest(event)
			if redirectPort == 0 {
				t.Fatal("missing redirect listener")
			}
			client := dialRedirect(t, sourcePort, redirectPort)
			defer client.Close()
			select {
			case err := <-callbacks.endpointErrors:
				if !errors.Is(err, ErrTCPRedirectOwner) || test.err != nil && !errors.Is(err, test.err) {
					t.Fatalf("rejection = %v, want ownership failure and %v", err, test.err)
				}
			case <-time.After(time.Second):
				t.Fatal("ownership rejection was not reported")
			}
			select {
			case <-callbacks.accepted:
				t.Fatal("unverified owner consumed the redirect")
			default:
			}
			bridge.mu.Lock()
			pending := bridge.tcpPending[event.ID] != nil
			bridge.mu.Unlock()
			if !pending {
				t.Fatal("rejected connection removed the pending redirect")
			}
		})
	}
}

func TestBridge_TCP_redirect_rejects_not_ready_and_allows_duplicate_source_port(t *testing.T) {
	callbacks := &tcpTestCallbacks{state: intercept.GenerationState{Generation: 3}}
	bridge := startTCPTestBridge(t, &tcpTestBackend{}, callbacks)
	t.Cleanup(func() { _ = bridge.Close() })
	event := nativeTCPConnectRequestEvent{
		ID: 81, PID: 41, ExecutablePath: `c:\games\game.exe`,
		Local: netip.MustParseAddrPort("127.0.0.1:41000"), Remote: netip.MustParseAddrPort("1.1.1.1:443"),
	}

	if port := bridge.tcpConnectRequest(event); port != 0 {
		t.Fatalf("not-ready redirect port = %d, want 0", port)
	}
	callbacks.mu.Lock()
	callbacks.state.Ready = true
	callbacks.mu.Unlock()
	firstPort := bridge.tcpConnectRequest(event)
	if firstPort == 0 {
		t.Fatal("ready redirect returned no listener port")
	}
	duplicate := event
	duplicate.ID++
	duplicate.Remote = netip.MustParseAddrPort("1.0.0.1:443")
	secondPort := bridge.tcpConnectRequest(duplicate)
	if secondPort == 0 || secondPort == firstPort {
		t.Fatalf("duplicate source-port listener = %d, first %d", secondPort, firstPort)
	}
}

func TestBridge_TCP_redirect_bounds_pending_listeners_and_releases_slot(t *testing.T) {
	callbacks := &tcpTestCallbacks{
		state:    intercept.GenerationState{Generation: 4, Ready: true},
		accepted: make(chan tcpCallbackRecord, 1),
	}
	bridge := startTCPTestBridge(t, &tcpTestBackend{}, callbacks)
	bridge.tcpRedirectSlots = make(chan struct{}, 1)
	t.Cleanup(func() { _ = bridge.Close() })
	firstSource := reserveTCPPort(t)
	firstPort := bridge.tcpConnectRequest(validTCPConnectRequest(85, firstSource))
	if firstPort == 0 {
		t.Fatal("first redirect returned no listener")
	}
	if port := bridge.tcpConnectRequest(validTCPConnectRequest(86, reserveTCPPort(t))); port != 0 {
		t.Fatalf("redirect over pending limit = %d, want 0", port)
	}
	client := dialRedirect(t, firstSource, firstPort)
	record := <-callbacks.accepted
	_ = client.Close()
	_ = record.flow.Close()
	deadline := time.Now().Add(time.Second)
	for len(bridge.tcpRedirectSlots) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(bridge.tcpRedirectSlots) != 0 {
		t.Fatal("accepted redirect did not release pending slot")
	}
	if port := bridge.tcpConnectRequest(validTCPConnectRequest(87, reserveTCPPort(t))); port == 0 {
		t.Fatal("redirect slot was not reusable")
	}
}

func TestBridge_redirect_callback_panic_stops_bridge_without_crashing(t *testing.T) {
	backend := &fakeNativeBackend{waitForContext: true}
	bridge := newTestBridge(t, backend)
	callbacks := panicTCPCallbacks{state: intercept.GenerationState{Generation: 5, Ready: true}}
	if err := bridge.Start(context.Background(), callbacks); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	sourcePort := reserveTCPPort(t)
	event := validTCPConnectRequest(91, sourcePort)
	redirectPort := bridge.tcpConnectRequest(event)
	if redirectPort == 0 {
		t.Fatal("tcpConnectRequest() returned no redirect port")
	}
	dialRedirect(t, sourcePort, redirectPort).Close()

	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := bridge.Wait(waitCtx)
	var callbackErr *NativeCallbackError
	if !errors.As(err, &callbackErr) || !strings.Contains(err.Error(), "callback panic") {
		t.Fatalf("Wait() error = %v, want callback panic", err)
	}
}

func TestBridge_redirect_callback_can_close_bridge(t *testing.T) {
	backend := &fakeNativeBackend{waitForContext: true}
	bridge := newTestBridge(t, backend)
	callbacks := &closeTCPCallbacks{
		bridge: bridge, state: intercept.GenerationState{Generation: 6, Ready: true},
		closed: make(chan error, 1),
	}
	if err := bridge.Start(context.Background(), callbacks); err != nil {
		t.Fatal(err)
	}
	sourcePort := reserveTCPPort(t)
	redirectPort := bridge.tcpConnectRequest(validTCPConnectRequest(92, sourcePort))
	if redirectPort == 0 {
		t.Fatal("tcpConnectRequest() returned no redirect port")
	}
	dialRedirect(t, sourcePort, redirectPort).Close()

	select {
	case err := <-callbacks.closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("TCP callback deadlocked while closing bridge")
	}
}

type panicTCPCallbacks struct {
	state intercept.GenerationState
}

func (callbacks panicTCPCallbacks) TCP(context.Context, intercept.NativeTCPFlow) error {
	panic("test callback")
}
func (panicTCPCallbacks) UDP(context.Context, intercept.NativeUDPEndpoint) error { return nil }
func (panicTCPCallbacks) EndpointError(error)                                    {}
func (callbacks panicTCPCallbacks) GenerationState() intercept.GenerationState {
	return callbacks.state
}

type closeTCPCallbacks struct {
	bridge *Bridge
	state  intercept.GenerationState
	closed chan error
}

func (callbacks *closeTCPCallbacks) TCP(context.Context, intercept.NativeTCPFlow) error {
	callbacks.closed <- callbacks.bridge.Close()
	return nil
}
func (*closeTCPCallbacks) UDP(context.Context, intercept.NativeUDPEndpoint) error { return nil }
func (*closeTCPCallbacks) EndpointError(error)                                    {}
func (callbacks *closeTCPCallbacks) GenerationState() intercept.GenerationState {
	return callbacks.state
}

func validTCPConnectRequest(id intercept.NativeID, sourcePort uint16) nativeTCPConnectRequestEvent {
	return nativeTCPConnectRequestEvent{
		ID: id, PID: 41, ExecutablePath: `c:\games\game.exe`,
		Local:  netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), sourcePort),
		Remote: netip.MustParseAddrPort("8.8.8.8:443"),
	}
}

func dialRedirect(t *testing.T, sourcePort, redirectPort uint16) net.Conn {
	t.Helper()
	dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(sourcePort)}}
	connection, err := dialer.DialContext(context.Background(), "tcp4", net.JoinHostPort("127.0.0.1", portString(redirectPort)))
	if err != nil {
		t.Fatal(err)
	}
	return connection
}

func reserveTCPPort(t *testing.T) uint16 {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return uint16(port)
}

func portString(port uint16) string {
	return fmt.Sprintf("%d", port)
}

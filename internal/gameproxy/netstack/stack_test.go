package netstack

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/sagernet/gvisor/pkg/tcpip"
	"github.com/sagernet/gvisor/pkg/tcpip/adapters/gonet"
	"github.com/sagernet/gvisor/pkg/tcpip/header"
)

func TestNewConfiguresOneIPv4NICWithSlash24AndDefaultRoute(t *testing.T) {
	network, err := New(Config{Address: netip.MustParseAddr("10.20.30.40"), MTU: 1400, QueueSize: 8, Generation: 2})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer network.Close()

	nics := network.stack.NICInfo()
	info, ok := nics[stackNICID]
	if len(nics) != 1 || !ok {
		t.Fatalf("NICInfo = %#v, want one NIC %d", nics, stackNICID)
	}
	if len(info.ProtocolAddresses) != 1 || info.ProtocolAddresses[0].Protocol != header.IPv4ProtocolNumber {
		t.Fatalf("protocol addresses = %#v, want one IPv4 address", info.ProtocolAddresses)
	}
	address := info.ProtocolAddresses[0].AddressWithPrefix
	if address.PrefixLen != 24 || address.Address.String() != "10.20.30.40" {
		t.Fatalf("address = %v/%d, want 10.20.30.40/24", address.Address, address.PrefixLen)
	}
	routes := network.stack.GetRouteTable()
	if len(routes) != 1 || routes[0].NIC != stackNICID || routes[0].Destination.Prefix() != 0 {
		t.Fatalf("routes = %#v, want one IPv4 default route", routes)
	}
}

func TestOpenUDPSendsToMultipleRemoteDestinations(t *testing.T) {
	client := newTestStack(t, "10.0.0.1", 9)
	server := newTestStack(t, "10.0.0.2", 9)
	stopLinks := connectTestStacks(t, client, server)
	defer stopLinks()

	first := openTestUDP(t, server, 9001)
	defer first.Close()
	second := openTestUDP(t, server, 9002)
	defer second.Close()
	packetConn, err := client.OpenUDP()
	if err != nil {
		t.Fatalf("OpenUDP: %v", err)
	}
	defer packetConn.Close()

	writeUDP(t, packetConn, "first", "10.0.0.2", 9001)
	writeUDP(t, packetConn, "second", "10.0.0.2", 9002)
	readUDP(t, first, "first")
	readUDP(t, second, "second")
}

func TestDialTCPReturnsContextCancellation(t *testing.T) {
	network := newTestStack(t, "10.0.0.1", 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := network.DialTCP(ctx, netip.MustParseAddrPort("10.0.0.2:80"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("DialTCP error = %v, want context.Canceled", err)
	}
}

func TestCloseUnblocksTCPDialAndUDPRead(t *testing.T) {
	network := newTestStack(t, "10.0.0.1", 1)
	packetConn, err := network.OpenUDP()
	if err != nil {
		t.Fatalf("OpenUDP: %v", err)
	}
	udpResult := make(chan error, 1)
	go func() {
		_, _, readErr := packetConn.ReadFrom(make([]byte, 16))
		udpResult <- readErr
	}()
	tcpResult := make(chan error, 1)
	go func() {
		_, dialErr := network.DialTCP(context.Background(), netip.MustParseAddrPort("10.0.0.2:80"))
		tcpResult <- dialErr
	}()

	network.Close()
	network.Close()
	if err := <-udpResult; err == nil {
		t.Fatal("UDP ReadFrom returned nil after stack close")
	}
	if err := <-tcpResult; err == nil {
		t.Fatal("TCP Dial returned nil after stack close")
	}
	if _, err := network.OpenUDP(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("OpenUDP after close = %v, want net.ErrClosed", err)
	}
}

func newTestStack(t *testing.T, address string, generation uint64) *Stack {
	t.Helper()
	network, err := New(Config{Address: netip.MustParseAddr(address), MTU: 1500, QueueSize: 64, Generation: generation})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(network.Close)
	return network
}

func openTestUDP(t *testing.T, network *Stack, port uint16) net.PacketConn {
	t.Helper()
	address := tcpip.FullAddress{NIC: stackNICID, Addr: network.address, Port: port}
	conn, err := gonet.DialUDP(network.stack, &address, nil, header.IPv4ProtocolNumber)
	if err != nil {
		t.Fatalf("DialUDP bind: %v", err)
	}
	return conn
}

func writeUDP(t *testing.T, conn net.PacketConn, payload, address string, port int) {
	t.Helper()
	if err := conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetWriteDeadline: %v", err)
	}
	written, err := conn.WriteTo([]byte(payload), &net.UDPAddr{IP: net.ParseIP(address), Port: port})
	if err != nil || written != len(payload) {
		t.Fatalf("WriteTo = (%d, %v), want (%d, nil)", written, err, len(payload))
	}
}

func readUDP(t *testing.T, conn net.PacketConn, expected string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buffer := make([]byte, 64)
	read, _, err := conn.ReadFrom(buffer)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if string(buffer[:read]) != expected {
		t.Fatalf("payload = %q, want %q", buffer[:read], expected)
	}
}

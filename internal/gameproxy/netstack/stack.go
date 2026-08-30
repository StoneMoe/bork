package netstack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"

	"github.com/sagernet/gvisor/pkg/tcpip"
	"github.com/sagernet/gvisor/pkg/tcpip/adapters/gonet"
	"github.com/sagernet/gvisor/pkg/tcpip/header"
	"github.com/sagernet/gvisor/pkg/tcpip/network/ipv4"
	gvisorstack "github.com/sagernet/gvisor/pkg/tcpip/stack"
	"github.com/sagernet/gvisor/pkg/tcpip/transport/tcp"
	"github.com/sagernet/gvisor/pkg/tcpip/transport/udp"
)

const stackNICID tcpip.NICID = 1

var ErrInvalidConfig = errors.New("netstack: invalid configuration")

type Config struct {
	Address    netip.Addr
	MTU        uint32
	QueueSize  int
	Generation uint64
}

type Stack struct {
	stack      *gvisorstack.Stack
	link       *LinkEndpoint
	address    tcpip.Address
	generation uint64

	mu          sync.Mutex
	closed      bool
	connections map[io.Closer]struct{}
	operations  sync.WaitGroup
	closeOnce   sync.Once
	closeDone   chan struct{}
}

func New(config Config) (*Stack, error) {
	if !config.Address.Is4() || config.MTU == 0 || config.QueueSize <= 0 {
		return nil, ErrInvalidConfig
	}
	link, err := NewLinkEndpoint(config.Generation, config.MTU, config.QueueSize)
	if err != nil {
		return nil, fmt.Errorf("create link endpoint: %w", err)
	}
	networkStack := gvisorstack.New(gvisorstack.Options{
		NetworkProtocols:   []gvisorstack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []gvisorstack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	if netstackErr := networkStack.CreateNIC(stackNICID, link); netstackErr != nil {
		link.Close()
		return nil, fmt.Errorf("create NIC: %s", netstackErr)
	}
	addressBytes := config.Address.As4()
	address := tcpip.AddrFrom4(addressBytes)
	protocolAddress := tcpip.ProtocolAddress{
		Protocol:          header.IPv4ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: address, PrefixLen: 24},
	}
	if netstackErr := networkStack.AddProtocolAddress(stackNICID, protocolAddress, gvisorstack.AddressProperties{}); netstackErr != nil {
		networkStack.Close()
		networkStack.Wait()
		return nil, fmt.Errorf("add IPv4 address: %s", netstackErr)
	}
	networkStack.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: stackNICID}})
	return &Stack{
		stack:       networkStack,
		link:        link,
		address:     address,
		generation:  config.Generation,
		connections: make(map[io.Closer]struct{}),
		closeDone:   make(chan struct{}),
	}, nil
}

func (network *Stack) LinkEndpoint() *LinkEndpoint { return network.link }

func (network *Stack) DialTCP(ctx context.Context, remote netip.AddrPort) (net.Conn, error) {
	if !remote.Addr().Is4() {
		return nil, ErrInvalidConfig
	}
	if !network.beginOperation() {
		return nil, net.ErrClosed
	}
	defer network.operations.Done()
	remoteAddress := remote.Addr().As4()
	connection, err := gonet.DialContextTCP(ctx, network.stack, tcpip.FullAddress{
		Addr: tcpip.AddrFrom4(remoteAddress),
		Port: remote.Port(),
	}, header.IPv4ProtocolNumber)
	if err != nil {
		if network.isClosed() {
			return nil, net.ErrClosed
		}
		return nil, err
	}
	tracked := newTrackedConn(connection, network.removeConnection)
	if !network.addConnection(tracked) {
		_ = tracked.Close()
		return nil, net.ErrClosed
	}
	return tracked, nil
}

func (network *Stack) OpenUDP() (net.PacketConn, error) {
	if !network.beginOperation() {
		return nil, net.ErrClosed
	}
	defer network.operations.Done()
	connection, err := gonet.DialUDP(network.stack, nil, nil, header.IPv4ProtocolNumber)
	if err != nil {
		return nil, fmt.Errorf("open UDP endpoint: %w", err)
	}
	tracked := newTrackedPacketConn(connection, network.removeConnection)
	if !network.addConnection(tracked) {
		_ = tracked.Close()
		return nil, net.ErrClosed
	}
	return tracked, nil
}

func (network *Stack) Close() {
	network.closeOnce.Do(func() {
		network.mu.Lock()
		network.closed = true
		connections := make([]io.Closer, 0, len(network.connections))
		for connection := range network.connections {
			connections = append(connections, connection)
		}
		network.mu.Unlock()

		network.link.Close()
		network.stack.Close()
		for _, connection := range connections {
			_ = connection.Close()
		}
		network.operations.Wait()
		network.stack.Wait()
		close(network.closeDone)
	})
	<-network.closeDone
}

func (network *Stack) beginOperation() bool {
	network.mu.Lock()
	defer network.mu.Unlock()
	if network.closed {
		return false
	}
	network.operations.Add(1)
	return true
}

func (network *Stack) isClosed() bool {
	network.mu.Lock()
	defer network.mu.Unlock()
	return network.closed
}

func (network *Stack) addConnection(connection io.Closer) bool {
	network.mu.Lock()
	defer network.mu.Unlock()
	if network.closed {
		return false
	}
	network.connections[connection] = struct{}{}
	return true
}

func (network *Stack) removeConnection(connection io.Closer) {
	network.mu.Lock()
	defer network.mu.Unlock()
	delete(network.connections, connection)
}

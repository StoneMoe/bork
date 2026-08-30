package netstack

import (
	"context"
	"errors"
	"net"
	"sync"

	"github.com/sagernet/gvisor/pkg/buffer"
	"github.com/sagernet/gvisor/pkg/tcpip"
	"github.com/sagernet/gvisor/pkg/tcpip/header"
	"github.com/sagernet/gvisor/pkg/tcpip/stack"
)

var (
	ErrInvalidIPv4     = errors.New("netstack: invalid IPv4 packet")
	ErrInvalidLink     = errors.New("netstack: invalid link endpoint configuration")
	ErrStaleGeneration = errors.New("netstack: stale generation")
)

// OutboundPacket is an owned raw IPv4 packet emitted by one stack generation.
type OutboundPacket struct {
	Generation uint64
	Data       []byte
}

// LinkEndpoint connects gVisor's IPv4 stack to Bork's raw-packet transport.
type LinkEndpoint struct {
	mu         sync.RWMutex
	dispatcher stack.NetworkDispatcher
	mtu        uint32
	generation uint64
	outbound   chan OutboundPacket
	closed     bool
}

var _ stack.LinkEndpoint = (*LinkEndpoint)(nil)

func NewLinkEndpoint(generation uint64, mtu uint32, queueSize int) (*LinkEndpoint, error) {
	if mtu == 0 || queueSize <= 0 {
		return nil, ErrInvalidLink
	}
	return &LinkEndpoint{
		mtu:        mtu,
		generation: generation,
		outbound:   make(chan OutboundPacket, queueSize),
	}, nil
}

func (endpoint *LinkEndpoint) MTU() uint32 {
	endpoint.mu.RLock()
	defer endpoint.mu.RUnlock()
	return endpoint.mtu
}

func (endpoint *LinkEndpoint) SetMTU(mtu uint32) {
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	endpoint.mtu = mtu
}

func (*LinkEndpoint) MaxHeaderLength() uint16 { return 0 }

func (*LinkEndpoint) LinkAddress() tcpip.LinkAddress { return "" }

func (*LinkEndpoint) SetLinkAddress(tcpip.LinkAddress) {}

func (*LinkEndpoint) Capabilities() stack.LinkEndpointCapabilities {
	return stack.CapabilityNone
}

func (endpoint *LinkEndpoint) Attach(dispatcher stack.NetworkDispatcher) {
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	if endpoint.closed {
		endpoint.dispatcher = nil
		return
	}
	endpoint.dispatcher = dispatcher
}

func (endpoint *LinkEndpoint) IsAttached() bool {
	endpoint.mu.RLock()
	defer endpoint.mu.RUnlock()
	return endpoint.dispatcher != nil
}

func (*LinkEndpoint) Wait() {}

func (*LinkEndpoint) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareNone
}

func (*LinkEndpoint) AddHeader(*stack.PacketBuffer) {}

func (*LinkEndpoint) ParseHeader(*stack.PacketBuffer) bool { return true }

func (*LinkEndpoint) SetOnCloseAction(func()) {}

func (endpoint *LinkEndpoint) WritePackets(packets stack.PacketBufferList) (int, tcpip.Error) {
	endpoint.mu.RLock()
	defer endpoint.mu.RUnlock()
	if endpoint.closed {
		return 0, &tcpip.ErrClosedForSend{}
	}

	written := 0
	for _, packet := range packets.AsSlice() {
		if uint32(packet.Size()) > endpoint.mtu {
			return written, &tcpip.ErrMessageTooLong{}
		}
		data := make([]byte, 0, packet.Size())
		for _, part := range packet.AsSlices() {
			data = append(data, part...)
		}
		select {
		case endpoint.outbound <- OutboundPacket{Generation: endpoint.generation, Data: data}:
			written++
		default:
			return written, &tcpip.ErrNoBufferSpace{}
		}
	}
	return written, nil
}

func (endpoint *LinkEndpoint) ReadPacket(ctx context.Context) (OutboundPacket, error) {
	select {
	case <-ctx.Done():
		return OutboundPacket{}, ctx.Err()
	default:
	}
	select {
	case packet, ok := <-endpoint.outbound:
		if !ok {
			return OutboundPacket{}, net.ErrClosed
		}
		return packet, nil
	case <-ctx.Done():
		return OutboundPacket{}, ctx.Err()
	}
}

func (endpoint *LinkEndpoint) InjectInbound(generation uint64, packet []byte) error {
	if generation != endpoint.generation {
		return ErrStaleGeneration
	}
	ipv4Header := header.IPv4(packet)
	if !ipv4Header.IsValid(len(packet)) || int(ipv4Header.TotalLength()) != len(packet) {
		return ErrInvalidIPv4
	}

	endpoint.mu.RLock()
	defer endpoint.mu.RUnlock()
	if endpoint.closed {
		return net.ErrClosed
	}
	if uint32(len(packet)) > endpoint.mtu {
		return ErrInvalidIPv4
	}
	if endpoint.dispatcher == nil {
		return nil
	}
	owned := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(packet)})
	defer owned.DecRef()
	endpoint.dispatcher.DeliverNetworkPacket(header.IPv4ProtocolNumber, owned)
	return nil
}

func (endpoint *LinkEndpoint) Close() {
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	if endpoint.closed {
		return
	}
	endpoint.closed = true
	endpoint.dispatcher = nil
	for {
		select {
		case <-endpoint.outbound:
		default:
			close(endpoint.outbound)
			return
		}
	}
}

package netstack

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/sagernet/gvisor/pkg/buffer"
	"github.com/sagernet/gvisor/pkg/tcpip"
	"github.com/sagernet/gvisor/pkg/tcpip/header"
	"github.com/sagernet/gvisor/pkg/tcpip/stack"
)

type captureDispatcher struct {
	packet []byte
}

type discardDispatcher struct{}

func (d *captureDispatcher) DeliverNetworkPacket(_ tcpip.NetworkProtocolNumber, packet *stack.PacketBuffer) {
	for _, part := range packet.AsSlices() {
		d.packet = append(d.packet, part...)
	}
}

func (*captureDispatcher) DeliverLinkPacket(tcpip.NetworkProtocolNumber, *stack.PacketBuffer) {}

func (*discardDispatcher) DeliverNetworkPacket(tcpip.NetworkProtocolNumber, *stack.PacketBuffer) {}

func (*discardDispatcher) DeliverLinkPacket(tcpip.NetworkProtocolNumber, *stack.PacketBuffer) {}

func TestLinkEndpointCopiesOutboundPacketAndTagsGeneration(t *testing.T) {
	endpoint, err := NewLinkEndpoint(7, 1500, 1)
	if err != nil {
		t.Fatalf("NewLinkEndpoint: %v", err)
	}
	defer endpoint.Close()

	original := []byte{1, 2, 3, 4}
	packet := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(original)})
	var packets stack.PacketBufferList
	packets.PushBack(packet)
	written, netstackErr := endpoint.WritePackets(packets)
	packet.AsSlices()[0][0] = 9
	packet.DecRef()
	if netstackErr != nil || written != 1 {
		t.Fatalf("WritePackets = (%d, %v), want (1, nil)", written, netstackErr)
	}

	outbound, err := endpoint.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	if outbound.Generation != 7 || outbound.Data[0] != 1 {
		t.Fatalf("outbound = %#v, want generation 7 and copied data", outbound)
	}
}

func TestLinkEndpointReturnsBackpressureErrorWhenQueueIsFull(t *testing.T) {
	endpoint, err := NewLinkEndpoint(1, 1500, 1)
	if err != nil {
		t.Fatalf("NewLinkEndpoint: %v", err)
	}
	defer endpoint.Close()

	first := newPacket(t, []byte{1})
	second := newPacket(t, []byte{2})
	defer first.DecRef()
	defer second.DecRef()
	var packets stack.PacketBufferList
	packets.PushBack(first)
	packets.PushBack(second)

	written, netstackErr := endpoint.WritePackets(packets)
	if written != 1 {
		t.Fatalf("written = %d, want 1", written)
	}
	if _, ok := netstackErr.(*tcpip.ErrNoBufferSpace); !ok {
		t.Fatalf("error = %T, want *tcpip.ErrNoBufferSpace", netstackErr)
	}
}

func TestLinkEndpointRuntimeMTURejectsOversizedPacket(t *testing.T) {
	endpoint, err := NewLinkEndpoint(1, 8, 1)
	if err != nil {
		t.Fatalf("NewLinkEndpoint: %v", err)
	}
	defer endpoint.Close()
	endpoint.SetMTU(4)
	packet := newPacket(t, []byte{1, 2, 3, 4, 5})
	defer packet.DecRef()
	var packets stack.PacketBufferList
	packets.PushBack(packet)

	written, netstackErr := endpoint.WritePackets(packets)
	if endpoint.MTU() != 4 || written != 0 {
		t.Fatalf("MTU/written = %d/%d, want 4/0", endpoint.MTU(), written)
	}
	if _, ok := netstackErr.(*tcpip.ErrMessageTooLong); !ok {
		t.Fatalf("error = %T, want *tcpip.ErrMessageTooLong", netstackErr)
	}
}

func TestLinkEndpointInjectInboundRejectsStaleAndCopiesValidIPv4(t *testing.T) {
	endpoint, err := NewLinkEndpoint(4, 1500, 1)
	if err != nil {
		t.Fatalf("NewLinkEndpoint: %v", err)
	}
	defer endpoint.Close()
	dispatcher := &captureDispatcher{}
	endpoint.Attach(dispatcher)
	packet := validIPv4Packet(24)

	if err := endpoint.InjectInbound(3, packet); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("stale injection error = %v, want ErrStaleGeneration", err)
	}
	if err := endpoint.InjectInbound(4, packet); err != nil {
		t.Fatalf("InjectInbound: %v", err)
	}
	packet[0] = 0
	if len(dispatcher.packet) != 24 || dispatcher.packet[0] != 0x45 {
		t.Fatalf("delivered packet was not an owned IPv4 copy: %x", dispatcher.packet)
	}
}

func TestLinkEndpointRejectsMalformedIPv4(t *testing.T) {
	endpoint, err := NewLinkEndpoint(1, 1500, 1)
	if err != nil {
		t.Fatalf("NewLinkEndpoint: %v", err)
	}
	defer endpoint.Close()
	endpoint.Attach(&captureDispatcher{})

	if err := endpoint.InjectInbound(1, []byte{0x45}); !errors.Is(err, ErrInvalidIPv4) {
		t.Fatalf("InjectInbound error = %v, want ErrInvalidIPv4", err)
	}
}

func TestLinkEndpointCloseWakesReaderAndDetaches(t *testing.T) {
	endpoint, err := NewLinkEndpoint(1, 1500, 1)
	if err != nil {
		t.Fatalf("NewLinkEndpoint: %v", err)
	}
	endpoint.Attach(&captureDispatcher{})
	result := make(chan error, 1)
	go func() {
		_, readErr := endpoint.ReadPacket(context.Background())
		result <- readErr
	}()

	endpoint.Close()
	if err := <-result; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("ReadPacket error = %v, want net.ErrClosed", err)
	}
	if endpoint.IsAttached() {
		t.Fatal("endpoint remains attached after Close")
	}
}

func TestLinkEndpointSynchronizesAttachDetachWithInjection(t *testing.T) {
	endpoint, err := NewLinkEndpoint(1, 1500, 1)
	if err != nil {
		t.Fatalf("NewLinkEndpoint: %v", err)
	}
	defer endpoint.Close()
	dispatcher := &discardDispatcher{}
	packet := validIPv4Packet(20)
	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		for range 1000 {
			endpoint.Attach(dispatcher)
			endpoint.Attach(nil)
		}
	}()
	go func() {
		defer workers.Done()
		<-start
		for range 1000 {
			if injectErr := endpoint.InjectInbound(1, packet); injectErr != nil {
				t.Errorf("InjectInbound: %v", injectErr)
				return
			}
		}
	}()
	close(start)
	workers.Wait()
}

func newPacket(t *testing.T, data []byte) *stack.PacketBuffer {
	t.Helper()
	return stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(data)})
}

func validIPv4Packet(size int) []byte {
	packet := make([]byte, size)
	packet[0] = 0x45
	binary.BigEndian.PutUint16(packet[2:4], uint16(size))
	packet[8] = 64
	packet[9] = uint8(header.UDPProtocolNumber)
	copy(packet[12:16], []byte{10, 0, 0, 1})
	copy(packet[16:20], []byte{10, 0, 0, 2})
	return packet
}

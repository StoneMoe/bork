package iwan

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"bork/internal/gameproxy/netstack"
)

func TestGeneration_readStackExitsWhenResultSendIsBlocked(t *testing.T) {
	network, err := netstack.New(netstack.Config{
		Address: netip.MustParseAddr("10.20.30.40"), MTU: 1400, QueueSize: 8, Generation: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(network.Close)
	current := &generation{network: network}
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan outboundEvent)
	current.workers.Add(1)
	go current.readStack(ctx, events)
	packetConn, err := network.OpenUDP()
	if err != nil {
		t.Fatal(err)
	}
	defer packetConn.Close()
	if _, err := packetConn.WriteTo([]byte("fill"), &net.UDPAddr{IP: net.IPv4(10, 20, 30, 50), Port: 9000}); err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan struct{})
	go func() {
		current.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stack reader survived cancellation while result send was blocked")
	}
}

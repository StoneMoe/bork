package netstack

import (
	"context"
	"errors"
	"net"
	"testing"
)

func connectTestStacks(t *testing.T, first, second *Stack) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 2)
	pump := func(source, destination *Stack) {
		defer func() { done <- struct{}{} }()
		for {
			packet, err := source.LinkEndpoint().ReadPacket(ctx)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed) {
					return
				}
				t.Errorf("ReadPacket: %v", err)
				return
			}
			if err := destination.LinkEndpoint().InjectInbound(destination.generation, packet.Data); err != nil {
				t.Errorf("InjectInbound: %v", err)
				return
			}
		}
	}
	go pump(first, second)
	go pump(second, first)
	return func() {
		cancel()
		<-done
		<-done
	}
}

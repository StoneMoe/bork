package iwan

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestSupervisor_replacesInactiveGenerationAndIgnoresLateOldACK(t *testing.T) {
	server := newFakeReferenceServer(t)
	supervisor, err := newSupervisor(testOptions(t, server), testTimings())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(supervisor.Stop)
	oldOpen, _ := startAndACK(t, supervisor, handshakeSpec{server: server, ack: ackSpec{
		token: Token{1, 1}, session: SessionID{1, 1, 1, 1}, mtu: 1400,
	}})
	oldSocket, err := supervisor.OpenUDP()
	if err != nil {
		t.Fatal(err)
	}

	var currentOpen receivedDatagram
	for {
		datagram := server.next(t)
		if PacketType(datagram.packet[0]) == TypeOpen {
			currentOpen = datagram
			break
		}
	}
	if currentOpen.peer.String() == oldOpen.peer.String() {
		t.Fatalf("replacement reused old UDP endpoint %s", oldOpen.peer)
	}
	lateACK := buildTestACK(ackSpec{token: Token{9, 9}, session: SessionID{9, 9, 9, 9}, mtu: 1200, xor: true})
	server.send(t, oldOpen.peer, lateACK)
	currentACK := buildTestACK(ackSpec{token: Token{2, 2}, session: SessionID{2, 2, 2, 2}, mtu: 1300, xor: true})
	server.send(t, currentOpen.peer, currentACK)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := supervisor.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	status := supervisor.Status()
	if status.Generation != 2 || status.MTU != 1300 {
		t.Fatalf("replacement status = %#v", status)
	}
	deadlineErr := oldSocket.SetReadDeadline(time.Now().Add(time.Second))
	if deadlineErr == nil {
		_, _, deadlineErr = oldSocket.ReadFrom(make([]byte, 1))
	}
	if deadlineErr == nil {
		t.Fatal("old generation stack socket survived replacement")
	}
}

func TestSupervisor_closesDuringAuthenticationWithoutRetry(t *testing.T) {
	server := newFakeReferenceServer(t)
	supervisor, err := newSupervisor(testOptions(t, server), testTimings())
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if open := server.next(t); PacketType(open.packet[0]) != TypeOpen {
		t.Fatalf("first packet type = %#x, want OPEN", open.packet[0])
	}
	supervisor.Stop()
	server.expectNone(t, 2*testTimings().openRetry)
	if status := supervisor.Status(); status.State != StateStopped {
		t.Fatalf("status after Stop = %#v", status)
	}
	if _, err := supervisor.OpenUDP(); !errors.Is(err, ErrNotReady) {
		t.Fatalf("OpenUDP after Stop = %v, want ErrNotReady", err)
	}
}

func TestSupervisor_normalShutdownSendsCloseAndClosesStackSockets(t *testing.T) {
	server := newFakeReferenceServer(t)
	supervisor, err := newSupervisor(testOptions(t, server), testTimings())
	if err != nil {
		t.Fatal(err)
	}
	_, session := startAndACK(t, supervisor, handshakeSpec{server: server, ack: ackSpec{
		token: Token{1, 2}, session: SessionID{3, 4, 5, 6}, mtu: 1400,
	}})
	packetConn, err := supervisor.OpenUDP()
	if err != nil {
		t.Fatal(err)
	}

	supervisor.Stop()
	deadlineErr := packetConn.SetDeadline(time.Now().Add(time.Second))
	if deadlineErr == nil {
		buffer := make([]byte, 1)
		_, _, deadlineErr = packetConn.ReadFrom(buffer)
	}
	if deadlineErr == nil {
		t.Fatal("old stack UDP socket remained usable after Stop")
	}
	for {
		datagram := server.next(t)
		control, parseErr := ParseControl(datagram.packet, session)
		if parseErr == nil && control.Type == TypeClose {
			return
		}
	}
}

func TestSupervisor_repeatedConcurrentStartStopHasNoSurvivingGeneration(t *testing.T) {
	server := newFakeReferenceServer(t)
	supervisor, err := newSupervisor(testOptions(t, server), testTimings())
	if err != nil {
		t.Fatal(err)
	}

	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 20 {
				if err := supervisor.Start(context.Background()); err != nil {
					t.Errorf("Start: %v", err)
					return
				}
				supervisor.Stop()
			}
		}()
	}
	workers.Wait()
	supervisor.Stop()
	if status := supervisor.Status(); status.State != StateStopped {
		t.Fatalf("final status = %#v", status)
	}
	server.drain()
	server.expectNone(t, 2*testTimings().openRetry)
}

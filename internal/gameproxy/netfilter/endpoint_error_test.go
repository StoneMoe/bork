//go:build game_proxy

package netfilter

import (
	"context"
	"testing"

	"bork/internal/gameproxy/intercept"
)

func TestBridge_endpointError_cleans_only_failed_protocol(t *testing.T) {
	for _, test := range []struct {
		name    string
		event   nativeEvent
		wantTCP bool
		wantUDP bool
	}{
		{name: "TCP", event: nativeEventTCPReceive, wantUDP: true},
		{name: "UDP", event: nativeEventUDPReceive, wantTCP: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakeNativeBackend{}
			bridge := newTestBridge(t, backend)
			if err := bridge.Start(context.Background(), callbackStub{}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = bridge.Close() })
			id := intercept.NativeID(77)
			metadata := udpTestMetadata(id)
			tcp := newTCPFlow(context.Background(), backend, metadata)
			udp := newUDPEndpoint(context.Background(), backend, metadata, testNativeUDPOptions())
			bridge.mu.Lock()
			bridge.flows[id] = tcp
			bridge.udpEndpoints[id] = udp
			bridge.mu.Unlock()

			failure := &NativeCallbackError{Event: test.event, ID: id, CleanupStatus: nativeStatusSuccess}
			bridge.endpointError(failure)

			bridge.mu.Lock()
			_, hasTCP := bridge.flows[id]
			_, hasUDP := bridge.udpEndpoints[id]
			bridge.mu.Unlock()
			if hasTCP != test.wantTCP || hasUDP != test.wantUDP {
				t.Fatalf("remaining TCP/UDP = %v/%v, want %v/%v", hasTCP, hasUDP, test.wantTCP, test.wantUDP)
			}
		})
	}
}

func TestBridge_endpointError_retries_failed_pending_UDP_cleanup_once(t *testing.T) {
	backend := &fakeNativeBackend{}
	bridge := newTestBridge(t, backend)
	if err := bridge.Start(context.Background(), callbackStub{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	id := intercept.NativeID(88)
	bridge.mu.Lock()
	bridge.udpSockets[id] = nativeUDPCreatedEvent{ID: id}
	bridge.mu.Unlock()

	bridge.endpointError(&NativeCallbackError{
		Event: nativeEventUDPCreated, ID: id,
		Status: nativeStatusFail, CleanupStatus: nativeStatusFail,
	})

	if got := backend.suspendSnapshot(); len(got) != 1 || got[0] != id {
		t.Fatalf("SuspendUDP calls = %v, want [%d]", got, id)
	}
}

func TestNativeEvent_protocol_classifies_all_events(t *testing.T) {
	for event := nativeEventTCPConnectRequest; event <= nativeEventUDPClosed; event++ {
		protocol := event.protocol()
		if protocol != nativeProtocolTCP && protocol != nativeProtocolUDP {
			t.Fatalf("event %v protocol = %d", event, protocol)
		}
	}
	if protocol := nativeEvent(0).protocol(); protocol != 0 {
		t.Fatalf("unknown event protocol = %d, want 0", protocol)
	}
}

package peer

import (
	"net/netip"
	"testing"
	"time"

	"bork/internal/networking/endpoint"
	"bork/internal/protocol"
)

func TestCandidateSessionRetriesHelloUntilAuthenticated(t *testing.T) {
	client, remoteIdentity, remoteHello := testHelloClient(t)
	network := client.roomNetwork.(*fakeRoomNetwork)
	address := netip.MustParseAddrPort("127.0.0.1:49001")
	client.handleHello(endpoint.Datagram{Data: remoteHello, From: address})
	before := countSentPacketType(t, network, protocol.PacketHello)
	if before != 1 {
		t.Fatalf("initial Hello responses = %d, want 1", before)
	}
	client.handleHello(endpoint.Datagram{Data: remoteHello, From: address})
	if immediate := countSentPacketType(t, network, protocol.PacketHello); immediate != before {
		t.Fatalf("immediate duplicate Hello responses = %d, want %d", immediate, before)
	}
	client.remotePeers[remoteIdentity.PeerID()].candidateSession.lastHelloSentAt = time.Now().Add(-helloInterval)
	client.sendPings()
	if after := countSentPacketType(t, network, protocol.PacketHello); after != before+1 {
		t.Fatalf("Hello responses after retry = %d, want %d", after, before+1)
	}
}

func countSentPacketType(t *testing.T, network *fakeRoomNetwork, packetType protocol.PacketType) int {
	t.Helper()
	network.mu.RLock()
	defer network.mu.RUnlock()
	count := 0
	for _, packet := range network.sentPackets {
		actual, _, err := protocol.ParsePrefix(packet)
		if err != nil {
			t.Fatal(err)
		}
		if actual == packetType {
			count++
		}
	}
	return count
}

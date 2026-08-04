package peer

import (
	"net/netip"
	"reflect"
	"testing"
	"time"

	"bork/internal/networking/discovery"
)

func TestTopologyMessageRoundTripPreservesLargeTopology(t *testing.T) {
	message := topologyMessage{
		generation:    42,
		audioStreamID: [16]byte{9},
		candidates: []netip.AddrPort{
			netip.MustParseAddrPort("198.51.100.10:4000"),
			netip.MustParseAddrPort("[2001:db8::10]:4001"),
		},
		neighbors: make([]topologyEntry, 80),
	}
	for index := range message.neighbors {
		entry := topologyEntry{peerID: [32]byte{byte(index + 1)}}
		if index%2 == 0 {
			entry.addresses = []netip.AddrPort{
				netip.AddrPortFrom(netip.AddrFrom4([4]byte{203, 0, 113, byte(index + 1)}), uint16(5000+index)),
			}
		} else {
			entry.addresses = []netip.AddrPort{
				netip.AddrPortFrom(netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, byte(index + 1)}), uint16(5000+index)),
				netip.AddrPortFrom(netip.AddrFrom4([4]byte{192, 0, 2, byte(index + 1)}), uint16(6000+index)),
			}
		}
		message.neighbors[index] = entry
	}

	payload, err := encodeTopologyMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeTopologyMessage(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.neighbors) != 80 || !reflect.DeepEqual(decoded, message) {
		t.Fatalf("decoded topology message differs from input: got %d neighbors", len(decoded.neighbors))
	}
}

func TestDecodeTopologyMessageRejectsMalformedTruncatedAndTrailingInput(t *testing.T) {
	message := topologyMessage{
		generation:    7,
		audioStreamID: [16]byte{8},
		candidates:    []netip.AddrPort{netip.MustParseAddrPort("192.0.2.1:1000")},
		neighbors: []topologyEntry{{
			peerID:    [32]byte{1},
			addresses: []netip.AddrPort{netip.MustParseAddrPort("[2001:db8::1]:2000")},
		}},
	}
	payload, err := encodeTopologyMessage(message)
	if err != nil {
		t.Fatal(err)
	}

	for length := 0; length < len(payload); length++ {
		if _, err := decodeTopologyMessage(payload[:length]); err == nil {
			t.Fatalf("accepted topology message truncated to %d bytes", length)
		}
	}

	malformedVersion := append([]byte(nil), payload...)
	malformedVersion[0]++
	malformedFamily := append([]byte(nil), payload...)
	malformedFamily[27] = 5
	zeroAudio := append([]byte(nil), payload...)
	clear(zeroAudio[9:25])
	zeroIdentity := append([]byte(nil), payload...)
	neighborOffset := 27 + 7 + 4
	clear(zeroIdentity[neighborOffset : neighborOffset+32])
	for name, candidate := range map[string][]byte{
		"version":        malformedVersion,
		"address family": malformedFamily,
		"zero audio":     zeroAudio,
		"zero identity":  zeroIdentity,
		"trailing data":  append(append([]byte(nil), payload...), 0),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeTopologyMessage(candidate); err == nil {
				t.Fatal("malformed topology message was accepted")
			}
		})
	}
}

func TestTopologyMessageRejectsZeroGeneration(t *testing.T) {
	if _, err := encodeTopologyMessage(topologyMessage{}); err == nil {
		t.Fatal("encoder accepted zero fields")
	}
	if _, err := encodeTopologyMessage(topologyMessage{generation: 1}); err == nil {
		t.Fatal("encoder accepted audio stream zero")
	}
	payload, err := encodeTopologyMessage(topologyMessage{generation: 1, audioStreamID: [16]byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	clear(payload[1:9])
	if _, err := decodeTopologyMessage(payload); err == nil {
		t.Fatal("decoder accepted generation zero")
	}
}

func TestMarshalTopologyMessageRejectsZeroAudioStream(t *testing.T) {
	client := &Client{}
	if _, err := client.marshalTopologyMessage(1, "", netip.AddrPort{}); err == nil {
		t.Fatal("marshaller accepted audio stream zero")
	}
}

func TestEqualTopologyGenerationRefreshesClaimsAndHints(t *testing.T) {
	client := testClient(t, func() roomNetwork { return newFakeRoomNetwork() })
	senderIdentity := testRemoteIdentity(t)
	neighborIdentity := testRemoteIdentity(t)
	path, err := NewPath(netip.MustParseAddrPort("198.51.100.1:9000"))
	if err != nil {
		t.Fatal(err)
	}
	sender := &RemotePeer{identity: senderIdentity, session: testPeeringSession(t, path)}
	client.remotePeers[senderIdentity.PeerID()] = sender
	candidate := netip.MustParseAddrPort("203.0.113.10:9100")
	neighborAddress := netip.MustParseAddrPort("203.0.113.11:9200")
	payload, err := encodeTopologyMessage(topologyMessage{
		generation: 9, audioStreamID: [16]byte{1}, candidates: []netip.AddrPort{candidate},
		neighbors: []topologyEntry{{peerID: rawPeerIdentity(neighborIdentity), addresses: []netip.AddrPort{neighborAddress}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	startedAt := time.Unix(1000, 0)
	lastRefresh := startedAt
	for lastRefresh = startedAt; !lastRefresh.After(startedAt.Add(40 * time.Second)); lastRefresh = lastRefresh.Add(topologyRefresh) {
		client.handleTopologySnapshotAt(sender, payload, lastRefresh)
	}
	lastRefresh = lastRefresh.Add(-topologyRefresh)
	claimExpiry := lastRefresh.Add(topologyClaimTTL)
	claim := client.topology[senderIdentity.PeerID()].neighbors[neighborIdentity.PeerID()]
	if sender.session.topologyReceivedGeneration != 9 || sender.session.audioStreamID != ([16]byte{1}) || !claim.Equal(claimExpiry) {
		t.Fatalf("refreshed topology state = generation %d stream %x claim %v", sender.session.topologyReceivedGeneration, sender.session.audioStreamID, claim)
	}
	for _, address := range []netip.AddrPort{candidate, neighborAddress} {
		hint, exists := client.discoveredAddresses[address]
		if !exists || hint.source != discovery.SourceTopology || !hint.expiresAt.Equal(lastRefresh.Add(topologyHintTTL)) {
			t.Fatalf("refreshed topology hint %s = %#v", address, hint)
		}
	}
	if client.expireDiscoveryHints(claimExpiry.Add(-time.Nanosecond)) {
		t.Fatal("refreshed hints expired before their renewed lease")
	}
	client.expireTopology(claimExpiry.Add(-time.Nanosecond))
	if _, exists := client.topology[senderIdentity.PeerID()].neighbors[neighborIdentity.PeerID()]; !exists {
		t.Fatal("refreshed claim expired before its renewed lease")
	}

	olderCandidate := netip.MustParseAddrPort("203.0.113.12:9300")
	older, err := encodeTopologyMessage(topologyMessage{
		generation: 8, audioStreamID: [16]byte{2}, candidates: []netip.AddrPort{olderCandidate},
	})
	if err != nil {
		t.Fatal(err)
	}
	client.handleTopologySnapshotAt(sender, older, lastRefresh.Add(topologyRefresh))
	if _, exists := client.discoveredAddresses[olderCandidate]; exists || sender.session.audioStreamID != ([16]byte{1}) {
		t.Fatal("older topology generation was applied")
	}
	client.expireTopology(claimExpiry)
	client.expireDiscoveryHints(claimExpiry)
	if _, exists := client.topology[senderIdentity.PeerID()].neighbors[neighborIdentity.PeerID()]; exists {
		t.Fatal("older generation refreshed the claim lease")
	}
	if _, exists := client.discoveredAddresses[candidate]; exists {
		t.Fatal("older generation refreshed the hint lease")
	}
}

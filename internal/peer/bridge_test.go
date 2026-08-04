package peer

import (
	"net/netip"
	"sort"
	"testing"
	"time"

	"bork/internal/identity"
	"bork/internal/networking/endpoint"
	"bork/internal/protocol"
)

func TestBridgePathToChoosesSmallestDirectIntermediary(t *testing.T) {
	localIdentity, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := testRemoteIdentity(t)
	intermediaries := []identity.Identity{testRemoteIdentity(t), testRemoteIdentity(t)}
	sort.Slice(intermediaries, func(i, j int) bool { return intermediaries[i].PeerID() < intermediaries[j].PeerID() })
	now := time.Now()
	client := &Client{
		localIdentity: localIdentity,
		remotePeers:   make(map[string]*RemotePeer),
		topology: map[string]*topologyPeer{
			target.PeerID(): {identity: target, lastSeen: now, neighbors: make(map[string]time.Time)},
		},
	}
	addresses := map[string]netip.AddrPort{
		intermediaries[0].PeerID(): netip.MustParseAddrPort("198.51.100.1:9000"),
		intermediaries[1].PeerID(): netip.MustParseAddrPort("198.51.100.2:9000"),
	}
	for _, intermediary := range intermediaries {
		path, _ := NewPath(addresses[intermediary.PeerID()])
		session := testPeeringSession(t, path)
		session.authenticated = true
		client.remotePeers[intermediary.PeerID()] = &RemotePeer{identity: intermediary, session: session}
		client.topology[intermediary.PeerID()] = &topologyPeer{
			identity: intermediary,
			lastSeen: now,
			neighbors: map[string]time.Time{
				target.PeerID(): now.Add(time.Second),
			},
		}
	}

	path, ok := client.bridgePathTo(target.PeerID(), now)
	if !ok || path.Intermediary() != rawPeerIdentity(intermediaries[0]) || path.Target() != rawPeerIdentity(target) || path.Address() != addresses[intermediaries[0].PeerID()] {
		t.Fatalf("selected bridge path = %+v, ok=%v", path, ok)
	}
	client.topology[intermediaries[0].PeerID()].neighbors[target.PeerID()] = now
	path, ok = client.bridgePathTo(target.PeerID(), now)
	if !ok || path.Intermediary() != rawPeerIdentity(intermediaries[1]) {
		t.Fatalf("expired claim selected bridge path = %+v, ok=%v", path, ok)
	}

	directPath, _ := NewPath(netip.MustParseAddrPort("198.51.100.3:9000"))
	directSession := testPeeringSession(t, directPath)
	directSession.authenticated = true
	client.remotePeers[target.PeerID()] = &RemotePeer{identity: target, session: directSession}
	if _, ok := client.bridgePathTo(target.PeerID(), now); ok {
		t.Fatal("bridge path was returned for a directly authenticated target")
	}
}

func TestEnqueueControlOnBridgeUsesCurrentIntermediarySession(t *testing.T) {
	client, _, _ := testHelloClient(t)
	network := client.roomNetwork.(*fakeRoomNetwork)
	intermediary := testRemoteIdentity(t)
	target := testRemoteIdentity(t)
	currentAddress := netip.MustParseAddrPort("198.51.100.10:9000")
	intermediaryPath, _ := NewPath(currentAddress)
	intermediarySession := testPeeringSession(t, intermediaryPath)
	intermediarySession.authenticated = true
	client.remotePeers[intermediary.PeerID()] = &RemotePeer{identity: intermediary, session: intermediarySession}
	path, err := NewBridgePath(netip.MustParseAddrPort("198.51.100.9:9000"), rawPeerIdentity(intermediary), rawPeerIdentity(target))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.sendControlOnPath(path, client.helloPacket); err != nil {
		t.Fatal(err)
	}
	if len(network.sent) != 1 || network.sent[0] != currentAddress {
		t.Fatalf("bridge destinations = %v", network.sent)
	}
	decoded, err := protocol.ParseBridge(network.sentPackets[0], client.roomTag, intermediarySession.sessionID, intermediarySession.ciphers.ControlSend)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Origin != rawPeerIdentity(client.localIdentity.Identity) || decoded.Target != rawPeerIdentity(target) {
		t.Fatalf("bridge endpoints = %x -> %x", decoded.Origin, decoded.Target)
	}
}

func TestHandleBridgePacketForwardsOnlyToDirectTarget(t *testing.T) {
	client, _, _ := testHelloClient(t)
	network := client.roomNetwork.(*fakeRoomNetwork)
	origin := testRemoteIdentity(t)
	target := testRemoteIdentity(t)
	originAddress := netip.MustParseAddrPort("198.51.100.20:9000")
	targetAddress := netip.MustParseAddrPort("198.51.100.21:9000")
	originPath, _ := NewPath(originAddress)
	targetPath, _ := NewPath(targetAddress)
	originSession := testPeeringSession(t, originPath)
	targetSession := testPeeringSession(t, targetPath)
	originSession.authenticated = true
	targetSession.authenticated = true
	client.remotePeers[origin.PeerID()] = &RemotePeer{identity: origin, session: originSession}
	client.remotePeers[target.PeerID()] = &RemotePeer{identity: target, session: targetSession}
	if _, err := targetSession.control.nextSendSequence(); err != nil {
		t.Fatal(err)
	}

	packet, err := protocol.MarshalBridge(
		client.roomTag,
		originSession.sessionID,
		1,
		rawPeerIdentity(origin),
		rawPeerIdentity(target),
		client.helloPacket,
		originSession.ciphers.ControlRecv,
	)
	if err != nil {
		t.Fatal(err)
	}
	client.handleBridgePacket(endpoint.Datagram{Data: packet, From: originAddress})
	if len(network.sent) != 1 || network.sent[0] != targetAddress {
		t.Fatalf("forwarded bridge destinations = %v", network.sent)
	}
	decoded, err := protocol.ParseBridge(network.sentPackets[0], client.roomTag, targetSession.sessionID, targetSession.ciphers.ControlSend)
	if err != nil {
		t.Fatal(err)
	}
	header, err := protocol.ParseEstablishedHeader(network.sentPackets[0])
	if err != nil || header.Sequence != 2 {
		t.Fatalf("forwarded bridge header = %#v, %v", header, err)
	}
	if decoded.Origin != rawPeerIdentity(origin) || decoded.Target != rawPeerIdentity(target) {
		t.Fatalf("forwarded bridge = %+v", decoded)
	}
}

func TestExpireTopologyInvalidatesFanout(t *testing.T) {
	now := time.Unix(1000, 0)
	client := &Client{
		remotePeers: make(map[string]*RemotePeer),
		topology: map[string]*topologyPeer{
			"retained": {
				lastSeen:  now,
				neighbors: map[string]time.Time{"expired-edge": now},
			},
			"expired-peer": {
				lastSeen:  now.Add(-knownPeerTTL - time.Second),
				neighbors: make(map[string]time.Time),
			},
		},
	}
	client.expireTopology(now)
	if !client.fanoutDirty || client.topology["expired-peer"] != nil || len(client.topology["retained"].neighbors) != 0 {
		t.Fatalf("topology expiry did not invalidate fanout: %#v", client.topology)
	}
}

package peer

import (
	"net/netip"
	"testing"
	"time"

	"bork/internal/identity"
	"bork/internal/invite"
	"bork/internal/networking"
	"bork/internal/networking/endpoint"
	"bork/internal/protocol"
)

func TestRoomDatagramRefreshesOnlyAuthenticatedDirectSource(t *testing.T) {
	roomInvite, err := invite.New("media liveness")
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewClient(roomInvite, networking.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := identity.New()
	if err != nil {
		t.Fatal(err)
	}
	forwarder, err := identity.New()
	if err != nil {
		t.Fatal(err)
	}

	forwarderAddress := netip.MustParseAddrPort("192.0.2.11:4000")
	forwarderPath, _ := NewPath(forwarderAddress)
	streamID := [16]byte{1}
	oldActivity := time.Date(2026, time.August, 17, 10, 0, 0, 0, time.UTC)
	senderSession := &PeeringSession{
		authenticated:             true,
		lastAuthenticatedPacketAt: oldActivity, audioStreamID: streamID,
	}
	forwarderSession := &PeeringSession{
		path: forwarderPath, authenticated: true,
		lastAuthenticatedPacketAt: oldActivity,
	}
	receiver.remotePeers[sender.PeerID()] = &RemotePeer{identity: sender.Identity, activeSession: senderSession}
	receiver.remotePeers[forwarder.PeerID()] = &RemotePeer{identity: forwarder.Identity, activeSession: forwarderSession}

	header := protocol.RoomDatagramHeader{
		Class: protocol.TrafficAudio, SenderID: rawPeerIdentity(sender.Identity),
		StreamID: streamID, Sequence: 1,
	}
	packet, err := protocol.MarshalRoomDatagram(
		receiver.roomTag, header, 1, []byte{1}, receiver.roomDatagramProtector, sender,
	)
	if err != nil {
		t.Fatal(err)
	}

	invalidPacket := append([]byte(nil), packet...)
	invalidPacket[len(invalidPacket)-1] ^= 1
	receiver.handleRoomDatagram(endpoint.Datagram{
		Data: invalidPacket, From: forwarderAddress, ReceivedAt: oldActivity.Add(time.Second),
	}, nil)
	if forwarderSession.lastAuthenticatedPacketAt != oldActivity {
		t.Fatal("invalid room datagram refreshed the direct source session")
	}

	receivedAt := oldActivity.Add(2 * time.Second)
	receiver.handleRoomDatagram(endpoint.Datagram{
		Data: packet, From: forwarderAddress, ReceivedAt: receivedAt,
	}, nil)
	if forwarderSession.lastAuthenticatedPacketAt != receivedAt {
		t.Fatal("valid room datagram did not refresh the direct source session")
	}
	if senderSession.lastAuthenticatedPacketAt != oldActivity {
		t.Fatal("forwarded room datagram refreshed the original sender session")
	}

	receiver.handleRoomDatagram(endpoint.Datagram{
		Data: packet, From: forwarderAddress, ReceivedAt: receivedAt.Add(time.Second),
	}, nil)
	if forwarderSession.lastAuthenticatedPacketAt != receivedAt {
		t.Fatal("replayed room datagram refreshed the direct source session")
	}

	forwarderSession.authenticated = false
	header.Sequence++
	packet, err = protocol.MarshalRoomDatagram(
		receiver.roomTag, header, 2, []byte{2}, receiver.roomDatagramProtector, sender,
	)
	if err != nil {
		t.Fatal(err)
	}
	receiver.handleRoomDatagram(endpoint.Datagram{
		Data: packet, From: forwarderAddress, ReceivedAt: receivedAt.Add(2 * time.Second),
	}, nil)
	if forwarderSession.authenticated || forwarderSession.lastAuthenticatedPacketAt != receivedAt {
		t.Fatal("room datagram restored an unavailable direct source session")
	}
}

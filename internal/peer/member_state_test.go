package peer

import (
	"encoding/binary"
	"net/netip"
	"strings"
	"testing"
)

func TestMemberStateCodecAndNicknameValidation(t *testing.T) {
	nickname, err := NormalizeNickname("  Alice  ")
	if err != nil || nickname != "Alice" {
		t.Fatalf("NormalizeNickname() = %q, %v", nickname, err)
	}
	if boundary := strings.Repeat("\U00010000", maxNicknameRunes); len(boundary) != maxNicknameBytes {
		t.Fatal("test nickname does not exercise both limits")
	} else if normalized, err := NormalizeNickname(boundary); err != nil || normalized != boundary {
		t.Fatalf("boundary nickname = %q, %v", normalized, err)
	}
	for name, invalid := range map[string]string{
		"invalid UTF-8":  string([]byte{0xff}),
		"too many runes": strings.Repeat("a", maxNicknameRunes+1),
		"control":        "ali\u0085ce",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NormalizeNickname(invalid); err == nil {
				t.Fatal("invalid nickname was accepted")
			}
		})
	}

	payload, err := encodeMemberState(memberState{generation: 42, nickname: " Alice ", muted: true, playbackMuted: true})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeMemberState(payload)
	if err != nil || decoded.generation != 42 || decoded.nickname != "Alice" || !decoded.muted || !decoded.playbackMuted || binary.BigEndian.Uint64(payload[1:9]) != 42 || payload[9] != 3 {
		t.Fatalf("decoded member state = %#v, %v", decoded, err)
	}

	badVersion := append([]byte(nil), payload...)
	badVersion[0]++
	zeroGeneration := append([]byte(nil), payload...)
	clear(zeroGeneration[1:9])
	badFlags := append([]byte(nil), payload...)
	badFlags[9] = 4
	for name, malformed := range map[string][]byte{
		"short":                  payload[:9],
		"version":                badVersion,
		"zero generation":        zeroGeneration,
		"unknown flags":          badFlags,
		"non-canonical nickname": append(append([]byte(nil), payload...), ' '),
		"invalid nickname":       append(append([]byte(nil), payload[:10]...), 0xff),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeMemberState(malformed); err == nil {
				t.Fatal("malformed member state was accepted")
			}
		})
	}
}

func TestMemberStateGenerationReplacementIsSessionScoped(t *testing.T) {
	client := testClient(t, func() roomNetwork { return newFakeRoomNetwork() })
	client.stateChanges = make(chan struct{}, 4)
	remoteIdentity := testRemoteIdentity(t)
	path, err := NewPath(netip.MustParseAddrPort("192.0.2.90:9000"))
	if err != nil {
		t.Fatal(err)
	}
	session := testPeeringSession(t, path)
	session.authenticated = true
	remote := &RemotePeer{identity: remoteIdentity, session: session}
	client.remotePeers[remoteIdentity.PeerID()] = remote
	message := func(generation uint64, nickname string, muted, playbackMuted bool) []byte {
		payload, encodeErr := encodeMemberState(memberState{generation: generation, nickname: nickname, muted: muted, playbackMuted: playbackMuted})
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		return payload
	}

	client.handleReliableMessage(remote, deliveredReliableMessage{channel: reliableChannelMemberState, payload: message(2, "Alice", true, true)})
	client.handleReliableMessage(remote, deliveredReliableMessage{channel: reliableChannelMemberState, payload: message(1, "stale", false, false)})
	client.handleReliableMessage(remote, deliveredReliableMessage{channel: reliableChannelMemberState, payload: message(2, "duplicate", false, false)})
	if session.remoteMemberState.nickname != "Alice" || !session.remoteMemberState.muted || !session.remoteMemberState.playbackMuted || len(client.stateChanges) != 1 {
		t.Fatalf("session member state = %#v, changes=%d", session.remoteMemberState, len(client.stateChanges))
	}
	snapshot, _ := client.StateSnapshot()
	if len(snapshot.RemotePeers) != 1 || snapshot.RemotePeers[0].Nickname != "Alice" || !snapshot.RemotePeers[0].Muted || !snapshot.RemotePeers[0].PlaybackMuted {
		t.Fatalf("remote peer snapshot = %#v", snapshot.RemotePeers)
	}

	replacement := testPeeringSession(t, path)
	replacement.authenticated = true
	remote.session = replacement
	client.handleReliableMessage(remote, deliveredReliableMessage{channel: reliableChannelMemberState, payload: message(1, "Bob", false, false)})
	if replacement.remoteMemberState.generation != 1 || replacement.remoteMemberState.nickname != "Bob" || len(client.stateChanges) != 2 {
		t.Fatalf("replacement session member state = %#v, changes=%d", replacement.remoteMemberState, len(client.stateChanges))
	}
}

func TestMemberStateQueueRetriesAndCoalescesDesiredUpdates(t *testing.T) {
	client := testClient(t, func() roomNetwork { return newFakeRoomNetwork() })
	remoteIdentity := testRemoteIdentity(t)
	path, err := NewPath(netip.MustParseAddrPort("192.0.2.91:9000"))
	if err != nil {
		t.Fatal(err)
	}
	session := testPeeringSession(t, path)
	session.authenticated = true
	client.remotePeers[remoteIdentity.PeerID()] = &RemotePeer{identity: remoteIdentity, session: session}

	session.reliable.queuedBytes = maxQueuedReliableBytes
	client.queueMemberStates()
	if session.memberStateSentGeneration != 0 || len(session.reliable.outbound) != 0 {
		t.Fatal("rejected member state queue was committed")
	}
	session.reliable.queuedBytes = 0
	client.queueMemberStates()
	if session.memberStateSentGeneration != 1 || len(session.reliable.outbound) != 1 || session.reliable.channels[reliableChannelMemberState].ordered {
		t.Fatal("default member state was not retried unordered")
	}
	client.queueMemberStates()
	if len(session.reliable.outbound) != 1 {
		t.Fatal("unchanged member state was queued twice")
	}

	if err := client.SetLocalMemberState("first", false, false); err != nil {
		t.Fatal(err)
	}
	if err := client.SetLocalMemberState(" second ", true, true); err != nil {
		t.Fatal(err)
	}
	if len(client.memberStateUpdates) != 1 {
		t.Fatalf("coalesced update signals = %d, want 1", len(client.memberStateUpdates))
	}
	<-client.memberStateUpdates
	client.applyDesiredMemberState()
	if client.localMemberState.generation != 2 || client.localMemberState.nickname != "second" || !client.localMemberState.muted || !client.localMemberState.playbackMuted ||
		session.memberStateSentGeneration != 2 || len(session.reliable.outbound) != 2 {
		t.Fatalf("local state = %#v, sent=%d, queued=%d", client.localMemberState, session.memberStateSentGeneration, len(session.reliable.outbound))
	}
	queued, err := decodeMemberState(session.reliable.outbound[1].payload)
	if err != nil || queued.generation != 2 || queued.nickname != "second" || !queued.muted || !queued.playbackMuted {
		t.Fatalf("queued member state = %#v, %v", queued, err)
	}
	if err := client.SetLocalMemberState("second", true, true); err != nil {
		t.Fatal(err)
	}
	client.applyDesiredMemberState()
	if client.localMemberState.generation != 2 || len(session.reliable.outbound) != 2 || len(client.memberStateUpdates) != 0 {
		t.Fatal("unchanged desired state changed generation or queue")
	}
}

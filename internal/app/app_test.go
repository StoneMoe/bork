package app

import (
	"testing"

	"bork/internal/config"
	"bork/internal/invite"
	"bork/internal/peer"
)

func TestSnapshotScopesPeerIDToRoom(t *testing.T) {
	application := NewApp(config.AppConfig{}, nil)
	if state := application.snapshot(); state.Room != nil {
		t.Fatal("lobby snapshot contains a room identity")
	}

	roomInvite, err := invite.New("test room")
	if err != nil {
		t.Fatal(err)
	}
	first, err := peer.NewClient(roomInvite, application.config.Network.Options(), nil)
	if err != nil {
		t.Fatal(err)
	}
	application.room = &roomSession{client: first}
	firstState := application.snapshot()
	if firstState.Room == nil || firstState.Room.PeerID != first.PeerID() {
		t.Fatal("active room snapshot is missing its PeerID")
	}

	application.room.stopping = true
	if state := application.snapshot(); state.Room != nil {
		t.Fatal("stopping room retained its PeerID")
	}
	application.room = nil
	second, err := peer.NewClient(roomInvite, application.config.Network.Options(), nil)
	if err != nil {
		t.Fatal(err)
	}
	application.room = &roomSession{client: second}
	if secondState := application.snapshot(); secondState.Room == nil || secondState.Room.PeerID == firstState.Room.PeerID {
		t.Fatal("new room membership reused the previous PeerID")
	}
}

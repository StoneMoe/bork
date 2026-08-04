package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bork/internal/config"
	"bork/internal/peer"
)

func TestAppRoomLifecycle(t *testing.T) {
	application := testApp(t)
	initial := application.GetSnapshot()
	if initial.PeerID == "" || initial.Room != nil {
		t.Fatalf("initial snapshot = %#v", initial)
	}
	if err := application.SetNickname("  Alice  "); err != nil {
		t.Fatal(err)
	}
	if nickname := application.GetSnapshot().Nickname; nickname != "Alice" {
		t.Fatalf("nickname = %q, want Alice", nickname)
	}
	if err := application.CreateRoom("Night Shift"); err != nil {
		t.Fatalf("CreateRoom() error = %v", err)
	}
	created := application.GetSnapshot()
	if created.Room == nil || created.Room.Name != "Night Shift" {
		t.Fatalf("created snapshot = %#v", created)
	}
	encodedInvite, err := application.GetInvite()
	if err != nil || encodedInvite == "" {
		t.Fatalf("GetInvite() = %q, %v", encodedInvite, err)
	}
	if err := application.LeaveRoom(); err != nil {
		t.Fatal(err)
	}
	left := application.GetSnapshot()
	if left.Room != nil {
		t.Fatalf("left snapshot = %#v", left)
	}
	if err := application.JoinRoom(encodedInvite); err != nil {
		t.Fatalf("JoinRoom() error = %v", err)
	}
	joined := application.GetSnapshot()
	if joined.Room == nil || joined.Room.Name != "Night Shift" {
		t.Fatalf("joined snapshot = %#v", joined)
	}
	if err := application.LeaveRoom(); err != nil {
		t.Fatal(err)
	}
}

func TestGetSnapshotWaitsForStartup(t *testing.T) {
	application := NewApp(config.Config{DataDir: t.TempDir(), STUNServers: []string{}}, testLogger())
	application.emit = func(context.Context, string, ...interface{}) {}
	result := make(chan struct{})
	go func() {
		application.GetSnapshot()
		close(result)
	}()
	select {
	case <-result:
		t.Fatal("GetSnapshot() returned before startup")
	case <-time.After(30 * time.Millisecond):
	}
	application.startup(context.Background())
	defer application.shutdown(context.Background())
	select {
	case <-result:
	case <-time.After(3 * time.Second):
		t.Fatal("GetSnapshot() remained blocked after startup")
	}
}

func TestStartupFailureIsReturnedBySnapshot(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(dataDir, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	application := NewApp(config.Config{DataDir: dataDir}, testLogger())
	application.emit = func(context.Context, string, ...interface{}) {}
	application.startup(context.Background())
	defer application.shutdown(context.Background())
	snapshot := application.GetSnapshot()
	if snapshot.Error == nil || snapshot.PeerID != "" {
		t.Fatalf("startup failure snapshot = %#v", snapshot)
	}
}

func TestStateNotificationsAreCoalesced(t *testing.T) {
	application := NewApp(config.Config{}, testLogger())
	events := make(chan uint64, 4)
	application.emit = func(_ context.Context, name string, data ...interface{}) {
		if name != stateChangedEvent || len(data) != 1 {
			t.Errorf("event = %q, %#v", name, data)
			return
		}
		revision, ok := data[0].(uint64)
		if !ok {
			t.Errorf("event revision = %#v", data[0])
			return
		}
		events <- revision
	}
	ctx, cancel := context.WithCancel(context.Background())
	application.startStateNotifier(ctx)
	application.markStateChanged()
	application.markStateChanged()
	application.markStateChanged()
	select {
	case revision := <-events:
		if revision != 3 {
			t.Fatalf("revision = %d, want 3", revision)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for state notification")
	}
	select {
	case revision := <-events:
		t.Fatalf("coalesced changes emitted another revision %d", revision)
	case <-time.After(2 * stateCoalesceInterval):
	}
	cancel()
	<-application.stateNotifierStopped
}

func TestShutdownJoinsNotifierBeforeReturning(t *testing.T) {
	application := NewApp(config.Config{}, testLogger())
	events := make(chan struct{}, 1)
	application.emit = func(context.Context, string, ...interface{}) { events <- struct{}{} }
	application.startStateNotifier(context.Background())
	application.markStateChanged()
	application.shutdown(context.Background())
	select {
	case <-events:
		t.Fatal("state notification was emitted during shutdown")
	case <-time.After(2 * stateCoalesceInterval):
	}
}

func TestNotifierCannotStartAfterCloseBegins(t *testing.T) {
	application := NewApp(config.Config{}, testLogger())
	application.stopStateNotifications()
	application.startStateNotifier(context.Background())
	application.stateMu.RLock()
	stop := application.stopStateNotifier
	application.stateMu.RUnlock()
	if stop != nil {
		t.Fatal("state notifier started after notifications were permanently closed")
	}
}

func TestEmptySnapshotSerializesCollectionsAsArrays(t *testing.T) {
	application := NewApp(config.Config{}, testLogger())
	encoded, err := json.Marshal(application.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, want := range []string{`"speakingPeerIds":[]`, `"captureDevices":[]`, `"playbackDevices":[]`, `"candidates":[]`, `"stun":[]`, `"knownAddresses":[]`} {
		if !strings.Contains(text, want) {
			t.Fatalf("snapshot JSON %s does not contain %s", text, want)
		}
	}
}

func TestRemoteMemberPresentationProjection(t *testing.T) {
	projected := projectRemotePeer(peer.RemotePeerSnapshot{PeerID: "peer", Nickname: "Bob", Muted: true})
	if projected.Nickname != "Bob" || !projected.Muted {
		t.Fatalf("projected remote peer = %#v", projected)
	}
}

func TestAppRejectsInvalidInviteAndShutdownCommands(t *testing.T) {
	application := testApp(t)
	if err := application.JoinRoom("not-an-invite"); err == nil {
		t.Fatal("JoinRoom() error = nil")
	}
	application.stateMu.Lock()
	application.shuttingDown = true
	application.stateMu.Unlock()
	if err := application.CreateRoom("too late"); err == nil {
		t.Fatal("CreateRoom() succeeded during shutdown")
	}
	application.stateMu.Lock()
	application.shuttingDown = false
	application.stateMu.Unlock()
}

func TestLeaveAndShutdownBothJoinStoppingRoom(t *testing.T) {
	application := NewApp(config.Config{}, testLogger())
	close(application.startupDone)
	roomContext, cancelRoom := context.WithCancel(context.Background())
	defer cancelRoom()
	room := &roomSession{cancel: cancelRoom, done: make(chan struct{})}
	application.stateMu.Lock()
	application.appContext = roomContext
	application.room = room
	application.stateMu.Unlock()

	leaveDone := make(chan error, 1)
	go func() { leaveDone <- application.LeaveRoom() }()
	deadline := time.After(time.Second)
	for {
		application.stateMu.RLock()
		stopping := room.stopping
		application.stateMu.RUnlock()
		if stopping {
			break
		}
		select {
		case <-deadline:
			t.Fatal("LeaveRoom() did not start room teardown")
		case <-time.After(time.Millisecond):
		}
	}
	shutdownDone := make(chan struct{})
	go func() {
		application.shutdown(context.Background())
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned before the stopping room exited")
	case <-time.After(30 * time.Millisecond):
	}
	close(room.done)
	if err := <-leaveDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not return after the stopping room exited")
	}
}

func TestOldRoomCannotChangeCurrentSnapshot(t *testing.T) {
	application := testApp(t)
	if err := application.CreateRoom("First"); err != nil {
		t.Fatal(err)
	}
	application.stateMu.RLock()
	oldRoom := application.room
	application.stateMu.RUnlock()
	oldClient, _ := application.activeClient()
	if err := application.LeaveRoom(); err != nil {
		t.Fatal(err)
	}
	if err := application.CreateRoom("Second"); err != nil {
		t.Fatal(err)
	}
	if current, _ := application.activeClient(); current == oldClient {
		t.Fatal("old client remained active")
	}
	application.publishRoomChange(roomStateChange{room: oldRoom})
	state := application.GetSnapshot()
	if state.Room == nil || state.Room.Name != "Second" {
		t.Fatalf("old room changed current snapshot: %#v", state)
	}
	if err := application.LeaveRoom(); err != nil {
		t.Fatal(err)
	}
}

func TestRoomGathersNetworkAndStops(t *testing.T) {
	application := testAppWithConfig(t, config.Config{
		DataDir:     t.TempDir(),
		UDPListen:   "127.0.0.1:0",
		STUNServers: []string{},
	})
	if err := application.CreateRoom("Network Room"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	for {
		snapshot := application.GetSnapshot()
		if snapshot.Diagnostics.ListenAddress != "" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("network did not start: %#v", snapshot.Diagnostics)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := application.LeaveRoom(); err != nil {
		t.Fatal(err)
	}
}

func TestNetworkFailureDetachesRoomAndKeepsDiagnostics(t *testing.T) {
	application := testAppWithConfig(t, config.Config{
		DataDir:     t.TempDir(),
		UDPListen:   "invalid-listen-address",
		STUNServers: []string{},
	})
	if err := application.CreateRoom("Broken Network"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	for {
		state := application.GetSnapshot()
		if state.Room == nil && state.Error != nil {
			if state.Diagnostics.NetworkError == "" || !strings.Contains(state.Error.Message, "invalid-listen-address") {
				t.Fatalf("terminal snapshot = %#v", state)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("failed room remained active: %#v", state)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func testApp(t *testing.T) *App {
	t.Helper()
	return testAppWithConfig(t, config.Config{DataDir: t.TempDir(), STUNServers: []string{}})
}

func testAppWithConfig(t *testing.T, cfg config.Config) *App {
	t.Helper()
	application := NewApp(cfg, testLogger())
	application.emit = func(context.Context, string, ...interface{}) {}
	application.startup(context.Background())
	if err := application.waitForStartup(); err != nil {
		application.shutdown(context.Background())
		t.Fatal(err)
	}
	t.Cleanup(func() { application.shutdown(context.Background()) })
	return application
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

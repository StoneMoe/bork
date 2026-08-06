package app

import (
	"bytes"
	"context"
	"encoding/base64"
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

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
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
	if created.Room.Transfers == nil {
		t.Fatal("room transfers projected as nil")
	}
	if created.Room.VirtualLAN.Status != "disabled" || created.Room.RemoteVirtualLAN == nil {
		t.Fatalf("room virtual LAN projection = %#v, %#v", created.Room.VirtualLAN, created.Room.RemoteVirtualLAN)
	}
	encodedRoom, err := json.Marshal(created.Room)
	if err != nil || !bytes.Contains(encodedRoom, []byte(`"remoteVirtualLAN":[]`)) {
		t.Fatalf("empty remote virtual LAN collection did not serialize as an array: %s, %v", encodedRoom, err)
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
	for _, want := range []string{`"captureMuted":false`, `"playbackMuted":false`, `"captureGain":100`, `"playbackGain":100`, `"echoCancellation":true`, `"noiseSuppression":true`, `"remoteLoudnessNormalization":true`, `"speakingPeerIds":[]`, `"captureDevices":[]`, `"playbackDevices":[]`, `"candidates":[]`, `"stun":[]`, `"discoveryHints":[]`} {
		if !strings.Contains(text, want) {
			t.Fatalf("snapshot JSON %s does not contain %s", text, want)
		}
	}
	if strings.Contains(text, `"muted":`) {
		t.Fatalf("snapshot JSON retained local muted alias: %s", text)
	}
}

func TestRemoteMemberPresentationProjection(t *testing.T) {
	projected := projectRemotePeer(peer.RemotePeerSnapshot{PeerID: "peer", Nickname: "Bob", Muted: true, PlaybackMuted: true, ScreenSharing: true})
	if projected.Nickname != "Bob" || !projected.Muted || !projected.PlaybackMuted || !projected.ScreenSharing {
		t.Fatalf("projected remote peer = %#v", projected)
	}
}

func TestFileTransferProjectionDoesNotLeakOutgoingPath(t *testing.T) {
	projected := projectTransfers([]peer.FileTransferSnapshot{
		{ID: "out", PeerID: "bob", Direction: "outgoing", Name: "send.txt", Path: `C:\\secret\\send.txt`, Status: "offered"},
		{ID: "in", PeerID: "bob", Direction: "incoming", Name: "receive.txt", Path: `C:\\saved\\receive.txt`, Status: "completed"},
	}, []peer.RemotePeerSnapshot{{PeerID: "bob", Nickname: "Bob"}})
	if projected == nil || len(projected) != 2 || projected[0].PeerNickname != "Bob" || projected[0].SavedPath != "" || projected[1].SavedPath == "" {
		t.Fatalf("projected transfers = %#v", projected)
	}
	encoded, err := json.Marshal(projected[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "savedPath") {
		t.Fatalf("outgoing transfer leaked source path: %s", encoded)
	}
	if empty := projectTransfers(nil, nil); empty == nil {
		t.Fatal("nil transfer snapshot projected as null")
	}
}

func TestVirtualLANProjectionAndSerialization(t *testing.T) {
	local := projectVirtualLAN(peer.VirtualLANSnapshot{Status: "error", Address: "100.64.1.2", Interface: "bork-test", Error: "permission denied"})
	remote := projectRemoteVirtualLAN(
		[]peer.RemoteVirtualLANSnapshot{{PeerID: "bob", Address: "100.64.1.3", Conflict: true}},
		[]peer.RemotePeerSnapshot{{PeerID: "bob", Nickname: "Bob"}},
	)
	encoded, err := json.Marshal(RoomState{VirtualLAN: local, RemoteVirtualLAN: remote})
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, want := range []string{`"status":"error"`, `"interface":"bork-test"`, `"error":"permission denied"`, `"nickname":"Bob"`, `"address":"100.64.1.3"`, `"conflict":true`} {
		if !strings.Contains(text, want) {
			t.Fatalf("virtual LAN projection JSON %s does not contain %s", text, want)
		}
	}
	if local.Address != "100.64.1.2" || remote[0].Nickname != "Bob" || !remote[0].Conflict {
		t.Fatalf("virtual LAN projections = %#v, %#v", local, remote)
	}
	if empty := projectRemoteVirtualLAN(nil, nil); empty == nil {
		t.Fatal("nil remote virtual LAN snapshots projected as null")
	}
}

func TestVirtualLANCommandsRequireRoom(t *testing.T) {
	application := testApp(t)
	if err := application.EnableVirtualLAN(); err == nil {
		t.Fatal("EnableVirtualLAN() outside a room succeeded")
	}
	if err := application.DisableVirtualLAN(); err == nil {
		t.Fatal("DisableVirtualLAN() outside a room succeeded")
	}
}

func TestWaitingClientCommandReleasesCommandMutex(t *testing.T) {
	application := testApp(t)
	if err := application.CreateRoom("Virtual LAN lock"); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- application.runWaitingClientCommand(func(*peer.Client) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	acquired := make(chan struct{})
	go func() {
		application.commandMu.Lock()
		application.commandMu.Unlock()
		close(acquired)
	}()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("waiting client command retained commandMu")
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestFileDialogsCanBeCancelled(t *testing.T) {
	application := testApp(t)
	if err := application.CreateRoom("Files"); err != nil {
		t.Fatal(err)
	}
	application.openFileDialog = func(context.Context, wailsruntime.OpenDialogOptions) (string, error) { return "", nil }
	if transferID, err := application.OfferFile("peer"); err != nil || transferID != "" {
		t.Fatalf("OfferFile() = %q, %v", transferID, err)
	}
}

func TestSafeFilename(t *testing.T) {
	for input, want := range map[string]string{`folder/name.txt`: "name.txt", `folder\\name.txt`: "name.txt", "": "download"} {
		if got := safeFilename(input); got != want {
			t.Fatalf("safeFilename(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestScreenVideoCaptureIDAndBase64RejectStaleWork(t *testing.T) {
	application := testApp(t)
	if err := application.CreateRoom("Screen"); err != nil {
		t.Fatal(err)
	}
	captureID, err := application.StartScreenShare(peer.ScreenVideoCodecH264Baseline, 1280, 720)
	if err != nil || captureID == 0 {
		t.Fatalf("StartScreenShare() = %d, %v", captureID, err)
	}
	if state := application.GetSnapshot(); state.Room == nil || !state.Room.ScreenSharing {
		t.Fatalf("sharing snapshot = %#v", state.Room)
	}
	videoData := []byte{0, 0, 0, 1, 0x65, 0x88, 0x84}
	encoded := base64.StdEncoding.EncodeToString(videoData)
	if _, err := application.SendScreenVideoChunk(captureID+1, 0, 66_667, true, encoded); err == nil {
		t.Fatal("stale capture sent a video chunk")
	}
	for name, malformed := range map[string]string{
		"whitespace": "AA\n=", "alphabet": "AA-_", "padding bits": "AB==",
		"oversized": strings.Repeat("A", base64.StdEncoding.EncodedLen(peer.MaxScreenVideoChunkBytes)+4),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := application.SendScreenVideoChunk(captureID, 0, 66_667, true, malformed); err == nil {
				t.Fatal("malformed base64 was accepted")
			}
		})
	}
	if _, err := application.SendScreenVideoChunk(captureID, 0, 66_667, true, encoded); err != nil {
		t.Fatal(err)
	}
	if _, err := application.SendScreenVideoChunk(captureID, 0, 0, false, encoded); err == nil {
		t.Fatal("invalid video duration was accepted")
	}
	if err := application.StopScreenShare(captureID + 1); err == nil {
		t.Fatal("stale capture stopped the current share")
	}
	if err := application.StopScreenShare(captureID); err != nil {
		t.Fatal(err)
	}
	if _, err := application.SendScreenVideoChunk(captureID, 0, 66_667, false, encoded); err == nil {
		t.Fatal("stopped capture sent a video chunk")
	}
	nextCaptureID, err := application.StartScreenShare(peer.ScreenVideoCodecH264Main, 640, 360)
	if err != nil || nextCaptureID == captureID {
		t.Fatalf("next StartScreenShare() = %d, %v", nextCaptureID, err)
	}
	if err := application.StopScreenShare(captureID); err == nil {
		t.Fatal("old capture ID stopped a newer share")
	}
	if err := application.LeaveRoom(); err != nil {
		t.Fatal(err)
	}
	if _, err := application.SendScreenVideoChunk(nextCaptureID, 0, 66_667, true, encoded); err == nil {
		t.Fatal("room leave retained capture state")
	}
}

func TestStopScreenShareZeroStopsRecoveredCaptureOnly(t *testing.T) {
	application := testApp(t)
	if err := application.CreateRoom("Recovered screen"); err != nil {
		t.Fatal(err)
	}
	captureID, err := application.StartScreenShare(peer.ScreenVideoCodecH264Baseline, 640, 360)
	if err != nil || captureID == 0 {
		t.Fatalf("StartScreenShare() = %d, %v", captureID, err)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte{0, 0, 0, 1, 0x65, 0x88})
	if _, err := application.SendScreenVideoChunk(0, 0, 66_667, true, encoded); err == nil {
		t.Fatal("zero capture ID sent a video chunk")
	}
	if err := application.StopScreenShare(captureID + 1); err == nil {
		t.Fatal("stale capture ID stopped the current share")
	}
	if state := application.GetSnapshot(); state.Room == nil || !state.Room.ScreenSharing {
		t.Fatalf("stale stop changed sharing snapshot = %#v", state.Room)
	}
	if err := application.StopScreenShare(0); err != nil {
		t.Fatalf("StopScreenShare(0) = %v", err)
	}
	if state := application.GetSnapshot(); state.Room == nil || state.Room.ScreenSharing {
		t.Fatalf("recovery stop retained sharing snapshot = %#v", state.Room)
	}
	if err := application.StopScreenShare(0); err == nil {
		t.Fatal("zero capture ID stopped a nonexistent share")
	}
}

func TestScreenVideoChunkUsesDirectEvent(t *testing.T) {
	application := NewApp(config.Config{}, testLogger())
	room := &roomSession{}
	application.appContext = context.Background()
	application.room = room
	events := make(chan ScreenVideoChunkEvent, 1)
	application.emit = func(_ context.Context, name string, data ...interface{}) {
		if name != screenVideoChunkEvent || len(data) != 1 {
			t.Errorf("event = %q, %#v", name, data)
			return
		}
		events <- data[0].(ScreenVideoChunkEvent)
	}
	videoData := []byte{0, 0, 0, 1, 0x65, 0x88}
	application.publishScreenVideoChunk(room, peer.ScreenVideoChunk{
		PeerID: "peer", SessionID: [16]byte{2}, Generation: 7, StreamID: [16]byte{1}, ChunkID: 9, Codec: peer.ScreenVideoCodecH264Baseline,
		Width: 1280, Height: 720, Timestamp: 123, Duration: 66_667, KeyFrame: true, Bytes: videoData,
	})
	event := <-events
	if event.PeerID != "peer" || event.SessionID != "02000000000000000000000000000000" || event.Generation != 7 || event.StreamID != "01000000000000000000000000000000" || event.ChunkID != 9 || event.Codec != peer.ScreenVideoCodecH264Baseline || event.Width != 1280 || event.Height != 720 || event.Timestamp != 123 || event.Duration != 66_667 || !event.KeyFrame || !bytes.Equal(event.Bytes, videoData) {
		t.Fatalf("screen event = %#v", event)
	}
	application.room = nil
	if encoded, err := json.Marshal(application.snapshot()); err != nil || bytes.Contains(encoded, videoData) {
		t.Fatal("screen video bytes leaked into AppSnapshot")
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

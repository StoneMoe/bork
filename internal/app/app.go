package app

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"time"

	"bork/internal/audio"
	"bork/internal/config"
	"bork/internal/globalkey"
	"bork/internal/media"
	"bork/internal/networking/discovery/tracker"
	"bork/internal/networking/endpoint"
	"bork/internal/peer"
	"bork/internal/protocol"
	"bork/internal/screenshare"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	stateCoalesceInterval = 50 * time.Millisecond
	audioRecoveryInterval = 2 * time.Second
)

var BuildVersion = "dev"

type App struct {
	config         config.AppConfig
	logger         *slog.Logger
	emit           func(context.Context, string, ...interface{})
	openFileDialog func(context.Context, wailsruntime.OpenDialogOptions) (string, error)
	saveFileDialog func(context.Context, wailsruntime.SaveDialogOptions) (string, error)

	commandMu sync.Mutex
	stateMu   sync.RWMutex

	appContext  context.Context
	startupDone chan struct{}

	statePending         chan struct{}
	stopStateNotifier    context.CancelFunc
	stateNotifierStopped chan struct{}
	notificationsClosed  bool

	nickname            string
	room                *roomSession
	audioEngine         *audio.Engine
	audioInitError      string
	stopAudioWatcher    context.CancelFunc
	audioWatcherDone    chan struct{}
	lastDiagnostics     Diagnostics
	nextScreenCaptureID uint32
	pushToTalk          *globalkey.Listener
	pushToTalkEnabled   bool
	pushToTalkKey       string
	shuttingDown        bool
}

type roomSession struct {
	client          *peer.Client
	media           *media.Flow
	cancel          context.CancelFunc
	done            chan struct{}
	remotePeerCount int
	screenVideo     *screenVideoRun
	screenAudio     *screenAudioRun
	stopping        bool
}

type screenVideoRun struct {
	id      uint32
	capture *screenshare.VideoCapture
	done    chan struct{}
}

type screenAudioRun struct {
	capture *screenshare.AudioCapture
	done    chan struct{}
}

type roomStateChange struct {
	room     *roomSession
	err      error
	terminal bool
}

func NewApp(cfg config.AppConfig, logger *slog.Logger) *App {
	if logger == nil {
		logger = slog.Default()
	}
	return &App{
		config:          cfg,
		logger:          logger,
		emit:            wailsruntime.EventsEmit,
		openFileDialog:  wailsruntime.OpenFileDialog,
		saveFileDialog:  wailsruntime.SaveFileDialog,
		startupDone:     make(chan struct{}),
		statePending:    make(chan struct{}, 1),
		lastDiagnostics: emptyDiagnostics(),
		pushToTalk:      globalkey.New(),
		pushToTalkKey:   globalkey.DefaultCode,
	}
}

func (a *App) startup(ctx context.Context) {
	a.stateMu.Lock()
	a.appContext = ctx
	a.stateMu.Unlock()
	a.startStateNotifier(ctx)

	a.commandMu.Lock()
	if err := a.initializeAudio(); err != nil {
		a.setAudioInitError(err)
	} else {
		a.setAudioInitError(nil)
	}
	a.startAudioWatcher(ctx)
	a.commandMu.Unlock()
	go a.watchPushToTalk(ctx)

	a.markStateChanged()
	close(a.startupDone)

}

func (a *App) waitForStartup() {
	<-a.startupDone
}

func (a *App) startStateNotifier(parent context.Context) {
	a.stateMu.Lock()
	if a.notificationsClosed || a.stopStateNotifier != nil {
		a.stateMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	a.stopStateNotifier = cancel
	a.stateNotifierStopped = done
	a.stateMu.Unlock()
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-a.statePending:
			}

			timer := time.NewTimer(stateCoalesceInterval)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return
			case <-timer.C:
			}
			draining := true
			for draining {
				select {
				case <-a.statePending:
				default:
					draining = false
				}
			}
			a.emit(ctx, stateChangedEvent)
		}
	}()
}

func (a *App) markStateChanged() {
	select {
	case a.statePending <- struct{}{}:
	default:
	}
}

func (a *App) publishRoomChange(change roomStateChange) {
	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	if !a.isActiveRoom(change.room) {
		return
	}
	if change.terminal {
		a.stopScreenVideoLocked(change.room)
		a.stopScreenAudioLocked(change.room)
		peerSnapshot, networkSnapshot := change.room.client.StateSnapshot()
		diagnostics := projectDiagnostics(networkSnapshot, peerSnapshot.Connectivity)
		a.stateMu.Lock()
		a.lastDiagnostics = diagnostics
		a.stateMu.Unlock()
		if change.err != nil {
			a.emitIssue(IssueTypeRoom, IssueLevelError, change.err)
		}
		a.stopPushToTalkLocked()
		room := a.detachActiveRoom()
		if room != nil {
			room.cancel()
		}
		if audioEngine, err := a.readyAudioEngine(); err == nil {
			audioEngine.Stop()
		}
		a.markStateChanged()
		return
	}
	a.reconcileAudioLocked(change.room)
	a.playPeerChangeLocked(change.room)
	a.markStateChanged()
}

func (a *App) initializeAudio() error {
	a.stateMu.RLock()
	existingAudioEngine := a.audioEngine
	shuttingDown := a.shuttingDown
	a.stateMu.RUnlock()
	if shuttingDown {
		return errors.New("application is shutting down")
	}
	if existingAudioEngine != nil {
		return nil
	}
	audioEngine, err := audio.New(audio.Options{MaxEncodedFrameBytes: protocol.MaxRoomDatagramPayload}, a.logger)
	if err != nil {
		a.logger.Warn("initialise voice audio", "error", err)
		return err
	}
	a.stateMu.Lock()
	a.audioEngine = audioEngine
	a.stateMu.Unlock()
	return nil
}

func (a *App) setAudioInitError(err error) {
	a.stateMu.Lock()
	if err == nil {
		a.audioInitError = ""
	} else {
		a.audioInitError = err.Error()
	}
	a.stateMu.Unlock()
}

func (a *App) startAudioWatcher(parent context.Context) {
	a.stateMu.Lock()
	if a.shuttingDown || a.audioEngine == nil || a.stopAudioWatcher != nil {
		a.stateMu.Unlock()
		return
	}
	audioEngine := a.audioEngine
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	a.stopAudioWatcher = cancel
	a.audioWatcherDone = done
	a.stateMu.Unlock()
	go func() {
		defer close(done)
		recovery := time.NewTicker(audioRecoveryInterval)
		defer recovery.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-audioEngine.StatusChanges():
				a.markStateChanged()
			case <-recovery.C:
				a.recoverAudioDevices(audioEngine)
			}
		}
	}()
}

func (a *App) recoverAudioDevices(audioEngine *audio.Engine) {
	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	if a.isShuttingDown() || audioEngine.Status().Running {
		return
	}
	a.stateMu.RLock()
	room := a.room
	a.stateMu.RUnlock()
	if room == nil || room.stopping {
		return
	}
	if err := a.syncAudioDevicesLocked(audioEngine, room); err != nil {
		a.logger.Debug("recover voice audio", "error", err)
	}
}

func (a *App) activateRoom(client *peer.Client) error {
	a.stateMu.RLock()
	parent := a.appContext
	a.stateMu.RUnlock()
	if parent == nil {
		return errors.New("application has not started")
	}
	ctx, cancel := context.WithCancel(parent)
	room := &roomSession{client: client, media: media.NewFlow(), cancel: cancel, done: make(chan struct{})}
	a.stateMu.Lock()
	a.room = room
	a.lastDiagnostics = emptyDiagnostics()
	a.stateMu.Unlock()
	go a.watchRoom(ctx, room)
	select {
	case <-client.Ready():
		a.reconcileAudioLocked(room)
		return nil
	case <-room.done:
		return errors.New("room stopped during startup")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *App) watchRoom(ctx context.Context, room *roomSession) {
	defer func() {
		a.stateMu.Lock()
		if a.room == room {
			a.room = nil
		}
		a.stateMu.Unlock()
		close(room.done)
		a.markStateChanged()
	}()
	peerChanges := room.client.StateChanges()
	screenVideoChunks := room.client.ScreenVideoChunks()
	peerResult := make(chan error, 1)
	go func() { peerResult <- room.client.Loop(ctx, room.media) }()
	for {
		select {
		case <-peerChanges:
			a.publishRoomChange(roomStateChange{room: room})
		case chunk := <-screenVideoChunks:
			a.publishScreenVideoChunk(room, chunk)
		case err := <-peerResult:
			a.publishRoomChange(roomStateChange{room: room, err: err, terminal: true})
			return
		case <-ctx.Done():
			<-peerResult
			return
		}
	}
}

func (a *App) publishScreenVideoChunk(room *roomSession, chunk peer.ScreenVideoChunk) {
	a.stateMu.RLock()
	active := a.room == room && room != nil && !room.stopping
	ctx := a.appContext
	a.stateMu.RUnlock()
	if active && ctx != nil {
		a.emit(ctx, screenVideoChunkEvent, ScreenVideoChunkEvent{
			PeerID: chunk.PeerID, SessionID: hex.EncodeToString(chunk.SessionID[:]), Generation: chunk.Generation, StreamID: hex.EncodeToString(chunk.StreamID[:]),
			ChunkID: chunk.ChunkID,
			Codec:   chunk.Codec, Width: chunk.Width, Height: chunk.Height,
			Timestamp: chunk.Timestamp, Duration: chunk.Duration, KeyFrame: chunk.KeyFrame, Bytes: chunk.Bytes,
		})
	}
}

func (a *App) runScreenVideo(room *roomSession, run *screenVideoRun) {
	info := run.capture.Info()
	a.stateMu.RLock()
	ctx := a.appContext
	a.stateMu.RUnlock()
	var runErr error
	defer func() {
		_ = run.capture.Close()
		// A command may wait for this goroutine while holding commandMu. Finish
		// the wait before reporting an asynchronous capture failure.
		close(run.done)
		a.commandMu.Lock()
		defer a.commandMu.Unlock()
		if room.screenVideo != run {
			return
		}
		room.screenVideo = nil
		a.stopScreenAudioLocked(room)
		_ = room.client.StopScreenShare()
		a.emit(ctx, screenPreviewEndedEvent, run.id)
		if runErr != nil && a.isActiveRoom(room) {
			a.emitIssue(IssueTypeScreen, IssueLevelError, errors.New("屏幕分享已停止: "+runErr.Error()))
			a.markStateChanged()
		}
	}()

	for {
		frame, err := run.capture.ReadFrame()
		if err != nil {
			runErr = err
			return
		}
		a.emit(ctx, screenPreviewChunkEvent, ScreenPreviewChunkEvent{
			CaptureID: run.id,
			Codec:     info.Codec,
			Width:     info.Width,
			Height:    info.Height,
			Timestamp: frame.Timestamp,
			Duration:  frame.Duration,
			KeyFrame:  frame.KeyFrame,
			Bytes:     frame.Payload,
		})
		sent, err := room.client.SendScreenVideoChunk(frame.Timestamp, frame.Duration, frame.KeyFrame, frame.Payload)
		if err != nil {
			runErr = err
			return
		}
		if !sent {
			_ = run.capture.ForceKeyFrame()
		}
	}
}

func (a *App) stopScreenVideoLocked(room *roomSession) {
	if room == nil || room.screenVideo == nil {
		return
	}
	run := room.screenVideo
	room.screenVideo = nil
	if err := run.capture.Close(); err != nil {
		a.logger.Debug("stop screen video capture", "error", err)
	}
	<-run.done
}

func (a *App) startScreenAudioLocked(room *roomSession) {
	capture, err := screenshare.StartAudioCapture(protocol.MaxRoomDatagramPayload)
	if err != nil {
		message := "屏幕声音不可用，已继续共享画面"
		if !errors.Is(err, screenshare.ErrUnsupported) {
			message += ": " + err.Error()
		}
		a.emitIssue(IssueTypeScreen, IssueLevelWarning, errors.New(message))
		a.markStateChanged()
		return
	}
	run := &screenAudioRun{capture: capture, done: make(chan struct{})}
	room.screenAudio = run
	go a.runScreenAudio(room, run)
}

func (a *App) runScreenAudio(room *roomSession, run *screenAudioRun) {
	var runErr error
	defer func() {
		_ = run.capture.Close()
		// stopScreenAudioLocked waits while holding commandMu, so release its
		// wait before reporting an asynchronous capture failure.
		close(run.done)
		a.commandMu.Lock()
		defer a.commandMu.Unlock()
		if room.screenAudio != run {
			return
		}
		room.screenAudio = nil
		if runErr != nil && a.isActiveRoom(room) {
			a.emitIssue(IssueTypeScreen, IssueLevelWarning, errors.New("屏幕声音已停止，画面仍在共享: "+runErr.Error()))
			a.markStateChanged()
		}
	}()
	for {
		frame, err := run.capture.ReadFrame()
		if err != nil {
			runErr = err
			return
		}
		if err := room.client.SendScreenAudioFrame(frame.Timestamp, frame.Payload); err != nil {
			runErr = err
			return
		}
	}
}

func (a *App) stopScreenAudioLocked(room *roomSession) {
	if room == nil || room.screenAudio == nil {
		return
	}
	run := room.screenAudio
	room.screenAudio = nil
	if err := run.capture.Close(); err != nil {
		a.logger.Debug("stop screen audio capture", "error", err)
	}
	<-run.done
}

func (a *App) detachActiveRoom() *roomSession {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.room == nil || a.room.stopping {
		return nil
	}
	a.room.stopping = true
	return a.room
}

func (a *App) stopRoom(room *roomSession) {
	if room == nil {
		return
	}
	room.cancel()
	<-room.done
}

func (a *App) isActiveRoom(room *roomSession) bool {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.room == room && room != nil && !room.stopping
}

func (a *App) reconcileAudioLocked(room *roomSession) {
	a.stateMu.RLock()
	audioEngine := a.audioEngine
	isActiveRoom := a.room == room && room != nil && !room.stopping
	a.stateMu.RUnlock()
	if audioEngine == nil || !isActiveRoom {
		return
	}
	status := audioEngine.Status()
	if status.Running || !status.DevicesAvailable() {
		return
	}
	if err := audioEngine.Start(room.media); err != nil {
		a.logger.Warn("start voice automatically", "error", err)
	}
}

func (a *App) playPeerChangeLocked(room *roomSession) {
	remotePeerCount := room.client.RemotePeerCount()
	previous := room.remotePeerCount
	room.remotePeerCount = remotePeerCount
	if remotePeerCount == previous {
		return
	}
	if audioEngine, err := a.readyAudioEngine(); err == nil {
		audioEngine.PlayPeerChange(remotePeerCount > previous)
	}
}

func (a *App) watchPushToTalk(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.pushToTalk.Changes():
			a.applyPushToTalk(a.pushToTalk.Pressed())
		case event := <-a.pushToTalk.Errors():
			a.handlePushToTalkError(event)
		}
	}
}

func (a *App) handlePushToTalkError(event globalkey.ListenerError) {
	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	if !a.pushToTalkEnabled || a.shuttingDown || !a.pushToTalk.IsCurrent(event) {
		return
	}
	a.stateMu.RLock()
	room := a.room
	a.stateMu.RUnlock()
	if room == nil || room.stopping {
		return
	}
	muted := true
	_ = a.setMutedLocked(&muted, nil, false)
	a.emitIssue(IssueTypeAudio, IssueLevelError, event)
	a.markStateChanged()
}

func (a *App) applyPushToTalk(pressed bool) {
	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	if a.shuttingDown || !a.pushToTalkEnabled {
		return
	}
	a.stateMu.RLock()
	room := a.room
	a.stateMu.RUnlock()
	if room == nil || room.stopping {
		return
	}
	muted := !pressed
	if err := a.setMutedLocked(&muted, nil, false); err != nil {
		a.emitIssue(IssueTypeAudio, IssueLevelError, err)
		a.markStateChanged()
	}
}

func (a *App) stopPushToTalkLocked() {
	if !a.pushToTalkEnabled {
		return
	}
	// Close the audio gate before waiting for an operating-system listener to
	// finish, so leaving a room is fail closed even if a key is held.
	if audioEngine, err := a.readyAudioEngine(); err == nil {
		audioEngine.SetCaptureMutedQuietly(true)
	}
	a.pushToTalk.Stop()
}

func (a *App) activeClient() (*peer.Client, error) {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	if a.room == nil || a.room.stopping {
		return nil, errors.New("not in a room")
	}
	return a.room.client, nil
}

func (a *App) snapshot() AppSnapshot {
	a.stateMu.RLock()
	nickname := a.nickname
	room := a.room
	if room != nil && room.stopping {
		room = nil
	}
	audioEngine := a.audioEngine
	audioInitError := a.audioInitError
	var diagnostics Diagnostics
	if room == nil {
		diagnostics = cloneDiagnostics(a.lastDiagnostics)
	}
	a.stateMu.RUnlock()

	state := AppSnapshot{
		Version:     BuildVersion,
		Nickname:    nickname,
		Audio:       emptyAudioStatus(),
		Diagnostics: diagnostics,
	}
	if audioEngine != nil {
		state.Audio = audioEngine.Status()
	} else if audioInitError != "" {
		state.Audio.Error = audioInitError
	}
	if room != nil {
		peerSnapshot, networkSnapshot := room.client.StateSnapshot()
		state.Room = &RoomState{
			Name:          peerSnapshot.Name,
			PeerID:        room.client.PeerID(),
			Phase:         peerSnapshot.Phase,
			ScreenSharing: peerSnapshot.ScreenSharing,
			RemotePeers:   make([]RemotePeer, 0, len(peerSnapshot.RemotePeers)),
			Transfers:     projectTransfers(peerSnapshot.Transfers, peerSnapshot.RemotePeers),
		}
		for _, remotePeer := range peerSnapshot.RemotePeers {
			state.Room.RemotePeers = append(state.Room.RemotePeers, projectRemotePeer(remotePeer))
		}
		state.Diagnostics = projectDiagnostics(networkSnapshot, peerSnapshot.Connectivity)
	}
	return state
}

func (a *App) emitIssue(issueType AppIssueType, level AppIssueLevel, err error) {
	if err == nil {
		return
	}
	a.emit(a.appContext, issueEvent, AppIssue{Type: issueType, Level: level, Message: err.Error()})
}

func (a *App) beforeClose(context.Context) bool {
	a.stopStateNotifications()
	return false
}

func (a *App) stopStateNotifications() {
	a.stateMu.Lock()
	stop, done := a.stopStateNotifier, a.stateNotifierStopped
	a.notificationsClosed = true
	a.stopStateNotifier, a.stateNotifierStopped = nil, nil
	a.stateMu.Unlock()
	if stop != nil {
		stop()
	}
	if done != nil {
		<-done
	}
}

func (a *App) shutdown(context.Context) {
	a.commandMu.Lock()
	a.stateMu.Lock()
	if a.shuttingDown {
		a.stateMu.Unlock()
		a.commandMu.Unlock()
		return
	}
	a.shuttingDown = true
	room := a.room
	if room != nil {
		room.stopping = true
	}
	a.stateMu.Unlock()
	if room != nil {
		// Start the peer shutdown before waiting for native capture teardown.
		room.cancel()
	}
	a.stopStateNotifications()
	a.stopPushToTalkLocked()
	a.stopScreenVideoLocked(room)
	a.stopScreenAudioLocked(room)
	if audioEngine, err := a.readyAudioEngine(); err == nil {
		audioEngine.Stop()
	}
	a.commandMu.Unlock()
	a.stopRoom(room)

	a.stateMu.Lock()
	stopAudioWatcher, audioWatcherDone := a.stopAudioWatcher, a.audioWatcherDone
	audioEngine := a.audioEngine
	a.stopAudioWatcher, a.audioWatcherDone = nil, nil
	a.audioEngine = nil
	a.stateMu.Unlock()
	if stopAudioWatcher != nil {
		stopAudioWatcher()
	}
	if audioWatcherDone != nil {
		<-audioWatcherDone
	}
	if audioEngine != nil {
		if err := audioEngine.Close(); err != nil {
			a.logger.Warn("close voice audio", "error", err)
		}
	}
}

func (a *App) readyAudioEngine() (*audio.Engine, error) {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	if a.audioEngine == nil {
		return nil, errors.New("voice audio is unavailable")
	}
	return a.audioEngine, nil
}

func (a *App) isShuttingDown() bool {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.shuttingDown
}

func emptyAudioStatus() audio.Status {
	return audio.Status{
		CaptureGain:                 100,
		PlaybackGain:                100,
		EchoCancellation:            true,
		NoiseSuppression:            true,
		RemoteLoudnessNormalization: true,
		SpeakingPeerIDs:             []string{},
		CaptureDevices:              []audio.Device{},
		PlaybackDevices:             []audio.Device{},
	}
}

func emptyDiagnostics() Diagnostics {
	return Diagnostics{
		Candidates: []endpoint.Candidate{}, STUN: []endpoint.STUNResult{}, Tracker: []tracker.ProviderStatus{},
		Connectivity: peer.ConnectivitySnapshot{DiscoveryHints: []peer.DiscoveryHintSnapshot{}},
	}
}

func cloneDiagnostics(value Diagnostics) Diagnostics {
	value.Candidates = append([]endpoint.Candidate{}, value.Candidates...)
	value.STUN = append([]endpoint.STUNResult{}, value.STUN...)
	value.Tracker = append([]tracker.ProviderStatus{}, value.Tracker...)
	for index := range value.Tracker {
		value.Tracker[index] = value.Tracker[index].Clone()
	}
	value.Connectivity.DiscoveryHints = append([]peer.DiscoveryHintSnapshot{}, value.Connectivity.DiscoveryHints...)
	return value
}

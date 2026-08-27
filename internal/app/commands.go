package app

import (
	"errors"
	"path/filepath"
	"strings"

	"bork/internal/audio"
	"bork/internal/globalkey"
	"bork/internal/identity"
	"bork/internal/invite"
	"bork/internal/peer"
	"bork/internal/screenshare"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

func (a *App) GetSnapshot() AppSnapshot {
	a.waitForStartup()
	return a.snapshot()
}

func (a *App) GetInvite() (string, error) {
	a.waitForStartup()
	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	client, err := a.activeClient()
	if err != nil {
		return "", err
	}
	return client.EncodedInvite(), nil
}

func (a *App) OfferFile(recipientPeerIDText string) (string, error) {
	recipientPeerID, err := identity.ParsePeerID(recipientPeerIDText)
	if err != nil {
		return "", err
	}
	a.waitForStartup()
	a.commandMu.Lock()
	if a.isShuttingDown() {
		a.commandMu.Unlock()
		return "", errors.New("application is shutting down")
	}
	client, err := a.activeClient()
	a.commandMu.Unlock()
	if err != nil {
		return "", err
	}
	a.stateMu.RLock()
	ctx := a.appContext
	a.stateMu.RUnlock()
	path, err := a.openFileDialog(ctx, wailsruntime.OpenDialogOptions{Title: "选择要发送的文件"})
	if err != nil || path == "" {
		return "", err
	}
	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	if !a.clientIsActive(client) {
		return "", errors.New("room changed while selecting a file")
	}
	return client.OfferFile(recipientPeerID, path)
}

func (a *App) AcceptFile(transferID string) error {
	a.waitForStartup()
	a.commandMu.Lock()
	if a.isShuttingDown() {
		a.commandMu.Unlock()
		return errors.New("application is shutting down")
	}
	client, err := a.activeClient()
	if err != nil {
		a.commandMu.Unlock()
		return err
	}
	peerSnapshot, _ := client.StateSnapshot()
	name := ""
	for _, transfer := range peerSnapshot.Transfers {
		if transfer.ID == transferID && transfer.Direction == "incoming" && transfer.Status == "offered" {
			name = transfer.Name
			break
		}
	}
	if name == "" {
		a.commandMu.Unlock()
		return errors.New("file offer is not pending")
	}
	a.commandMu.Unlock()
	a.stateMu.RLock()
	ctx := a.appContext
	a.stateMu.RUnlock()
	path, err := a.saveFileDialog(ctx, wailsruntime.SaveDialogOptions{Title: "保存接收的文件", DefaultFilename: safeFilename(name)})
	if err != nil || path == "" {
		return err
	}
	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	if !a.clientIsActive(client) {
		return errors.New("room changed while selecting a destination")
	}
	return client.AcceptFile(transferID, path)
}

func (a *App) clientIsActive(client *peer.Client) bool {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return !a.shuttingDown && a.room != nil && !a.room.stopping && a.room.client == client
}

func (a *App) RejectFile(transferID string) error {
	return a.runClientCommand(func(client *peer.Client) error { return client.RejectFile(transferID) })
}

func (a *App) CancelFile(transferID string) error {
	return a.runClientCommand(func(client *peer.Client) error { return client.CancelFile(transferID) })
}

func (a *App) runClientCommand(command func(*peer.Client) error) error {
	a.waitForStartup()
	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	if a.isShuttingDown() {
		return errors.New("application is shutting down")
	}
	client, err := a.activeClient()
	if err != nil {
		return err
	}
	return command(client)
}

func (a *App) runAudioCommand(command func(*audio.Engine) error) error {
	a.waitForStartup()
	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	if a.isShuttingDown() {
		return errors.New("application is shutting down")
	}
	engine, err := a.readyAudioEngine()
	if err != nil {
		return err
	}
	if err := command(engine); err != nil {
		return err
	}
	return nil
}

func safeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == string(filepath.Separator) || name == "" {
		return "download"
	}
	return name
}

func (a *App) CreateRoom(displayName string) error {
	a.waitForStartup()
	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	roomInvite, err := invite.New(displayName)
	if err != nil {
		return err
	}
	return a.createRoom(roomInvite)
}

func (a *App) JoinRoom(encodedInvite string) error {
	a.waitForStartup()
	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	return a.joinRoom(encodedInvite)
}

func (a *App) joinRoom(encodedInvite string) error {
	if strings.TrimSpace(encodedInvite) == "" {
		return errors.New("room invite is required")
	}
	roomInvite, err := invite.Parse(encodedInvite)
	if err != nil {
		return err
	}
	return a.createRoom(roomInvite)
}

func (a *App) createRoom(roomInvite invite.Invite) error {
	a.stateMu.RLock()
	nickname := a.nickname
	audioEngine := a.audioEngine
	hasRoom := a.room != nil
	shuttingDown := a.shuttingDown
	a.stateMu.RUnlock()
	if shuttingDown {
		return errors.New("application is shutting down")
	}
	if hasRoom {
		return errors.New("leave the current room before joining another")
	}
	client, err := peer.NewClient(roomInvite, a.config.Network.Options(), a.logger)
	if err != nil {
		return err
	}
	captureMuted := false
	playbackMuted := false
	if audioEngine != nil {
		if a.pushToTalkEnabled {
			audioEngine.SetCaptureMutedQuietly(true)
		}
		status := audioEngine.Status()
		captureMuted = status.CaptureMuted
		playbackMuted = status.PlaybackMuted
	}
	if err := client.SetLocalMemberState(nickname, captureMuted, playbackMuted); err != nil {
		return err
	}
	if err := a.activateRoom(client); err != nil {
		return err
	}
	if a.pushToTalkEnabled {
		if err := a.pushToTalk.Start(a.pushToTalkKey); err != nil {
			a.emitIssue(IssueTypeAudio, IssueLevelError, err)
		}
	}
	a.markStateChanged()
	return nil
}

func (a *App) LeaveRoom() error {
	a.waitForStartup()
	a.commandMu.Lock()
	if a.isShuttingDown() {
		a.commandMu.Unlock()
		return errors.New("application is shutting down")
	}
	room := a.detachActiveRoom()
	if room != nil {
		// Let the peer loop publish Leave while native capture is unwinding.
		room.cancel()
	}
	a.stopPushToTalkLocked()
	a.stopScreenVideoLocked(room)
	a.stopScreenAudioLocked(room)
	if audioEngine, err := a.readyAudioEngine(); err == nil {
		audioEngine.Stop()
	}
	a.stateMu.Lock()
	a.lastDiagnostics = emptyDiagnostics()
	a.stateMu.Unlock()
	a.markStateChanged()
	a.commandMu.Unlock()
	a.stopRoom(room)
	return nil
}

func (a *App) ListScreenSources() ([]screenshare.Source, error) {
	return screenshare.Sources()
}

func (a *App) StartScreenShare(sourceID string) (uint32, error) {
	a.waitForStartup()
	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	if a.isShuttingDown() {
		return 0, errors.New("application is shutting down")
	}
	a.stateMu.RLock()
	room := a.room
	a.stateMu.RUnlock()
	if room == nil || room.stopping {
		return 0, errors.New("not in a room")
	}
	if room.screenVideo != nil {
		return 0, errors.New("screen sharing is already active")
	}
	capture, err := screenshare.StartVideoCapture(
		sourceID,
		peer.MaxScreenVideoChunkBytes,
		peer.MaxScreenVideoWidth,
		peer.MaxScreenVideoHeight,
	)
	if err != nil {
		return 0, err
	}
	info := capture.Info()
	if err := room.client.StartScreenShare(info.Codec, info.Width, info.Height); err != nil {
		_ = capture.Close()
		return 0, err
	}
	a.nextScreenCaptureID++
	run := &screenVideoRun{id: a.nextScreenCaptureID, capture: capture, done: make(chan struct{})}
	room.screenVideo = run
	a.startScreenAudioLocked(room)
	go a.runScreenVideo(room, run)
	return run.id, nil
}

func (a *App) StopScreenShare(captureID uint32) error {
	a.waitForStartup()
	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	if a.isShuttingDown() {
		return errors.New("application is shutting down")
	}
	a.stateMu.RLock()
	room := a.room
	a.stateMu.RUnlock()
	// Zero is reserved for frontend recovery; concrete IDs still cannot stop a newer capture.
	if room == nil || room.stopping || room.screenVideo == nil || (captureID != 0 && room.screenVideo.id != captureID) {
		return errors.New("screen capture is stale")
	}
	if err := room.client.StopScreenShare(); err != nil {
		return err
	}
	a.stopScreenVideoLocked(room)
	a.stopScreenAudioLocked(room)
	return nil
}

// SetScreenAudioSource follows a remote screen's sound. The frontend sends an
// empty source peer ID while showing the local preview or no screen. The room
// peer ID prevents a delayed call from changing the next room.
func (a *App) SetScreenAudioSource(roomPeerIDText, sourcePeerIDText string) error {
	roomPeerID, err := identity.ParsePeerID(roomPeerIDText)
	if err != nil {
		return err
	}
	var sourcePeerID identity.PeerID
	if sourcePeerIDText != "" {
		sourcePeerID, err = identity.ParsePeerID(sourcePeerIDText)
		if err != nil {
			return err
		}
	}
	a.waitForStartup()
	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	if a.isShuttingDown() {
		return errors.New("application is shutting down")
	}
	a.stateMu.RLock()
	room := a.room
	a.stateMu.RUnlock()
	if room == nil || room.stopping || room.client.PeerID() != roomPeerID {
		return nil
	}
	room.media.SetScreenAudioSource(sourcePeerID)
	return nil
}

func (a *App) SetCaptureMuted(muted bool) error {
	return a.setMuted(&muted, nil)
}

func (a *App) SetPlaybackMuted(muted bool) error {
	return a.setMuted(nil, &muted)
}

func (a *App) setMuted(captureMuted, playbackMuted *bool) error {
	a.waitForStartup()
	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	if a.isShuttingDown() {
		return errors.New("application is shutting down")
	}
	if captureMuted != nil && !*captureMuted && a.pushToTalkEnabled {
		return errors.New("disable push-to-talk before unmuting the microphone")
	}
	return a.setMutedLocked(captureMuted, playbackMuted, true)
}

func (a *App) setMutedLocked(captureMuted, playbackMuted *bool, notify bool) error {
	audioEngine, err := a.readyAudioEngine()
	if err != nil {
		return err
	}
	if captureMuted != nil {
		if notify {
			audioEngine.SetCaptureMuted(*captureMuted)
		} else {
			audioEngine.SetCaptureMutedQuietly(*captureMuted)
		}
	}
	if playbackMuted != nil {
		audioEngine.SetPlaybackMuted(*playbackMuted)
	}
	status := audioEngine.Status()
	a.stateMu.RLock()
	nickname := a.nickname
	room := a.room
	a.stateMu.RUnlock()
	a.markStateChanged()
	if room != nil && !room.stopping {
		if err := room.client.SetLocalMemberState(nickname, status.CaptureMuted, status.PlaybackMuted); err != nil {
			return err
		}
	}
	return nil
}

// ConfigurePushToTalk stores the frontend preference. The operating-system
// listener is started only while a room is active.
func (a *App) ConfigurePushToTalk(enabled bool, code string) error {
	a.waitForStartup()
	if !globalkey.ValidCode(code) {
		return errors.New("push-to-talk key is unsupported")
	}
	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	if a.isShuttingDown() {
		return errors.New("application is shutting down")
	}
	audioEngine, audioErr := a.readyAudioEngine()
	if audioErr != nil {
		if enabled {
			return audioErr
		}
		a.pushToTalk.Stop()
		a.pushToTalkEnabled = false
		a.pushToTalkKey = code
		return nil
	}
	return a.configurePushToTalkLocked(audioEngine, enabled, code)
}

func (a *App) configurePushToTalkLocked(audioEngine *audio.Engine, enabled bool, code string) error {
	a.stateMu.RLock()
	room := a.room
	a.stateMu.RUnlock()
	activeRoom := room != nil && !room.stopping
	previousMuted := audioEngine.Status().CaptureMuted
	muted := previousMuted
	if enabled {
		muted = true
	} else if a.pushToTalkEnabled {
		muted = false
	}
	if err := a.setPushToTalkListenerLocked(audioEngine, enabled, activeRoom, code, previousMuted); err != nil {
		return err
	}
	a.pushToTalkEnabled = enabled
	a.pushToTalkKey = code
	if err := a.setMutedLocked(&muted, nil, false); err != nil {
		a.emitIssue(IssueTypeAudio, IssueLevelError, err)
	}
	return nil
}

func (a *App) setPushToTalkListenerLocked(audioEngine *audio.Engine, enabled, activeRoom bool, code string, previousMuted bool) error {
	if !enabled || !activeRoom {
		a.pushToTalk.Stop()
		return nil
	}
	// Mute before replacing the listener. This also covers a held old key
	// while the platform waits for its old callback source to stop.
	audioEngine.SetCaptureMutedQuietly(true)
	if err := a.pushToTalk.Start(code); err != nil {
		a.restoreMutedAfterPushToTalkFailure(previousMuted)
		return err
	}
	return nil
}

func (a *App) restoreMutedAfterPushToTalkFailure(previousMuted bool) {
	// A failed first enable restores the manual mute state. A failed rebind
	// keeps an existing PTT setup closed until its key is pressed again.
	muted := previousMuted || a.pushToTalkEnabled
	if err := a.setMutedLocked(&muted, nil, false); err != nil {
		a.emitIssue(IssueTypeAudio, IssueLevelError, err)
	}
}

func (a *App) SetCaptureGain(gain int) error {
	return a.runAudioCommand(func(engine *audio.Engine) error { return engine.SetCaptureGain(gain) })
}

func (a *App) SetPlaybackGain(gain int) error {
	return a.runAudioCommand(func(engine *audio.Engine) error { return engine.SetPlaybackGain(gain) })
}

func (a *App) SetRemoteLoudnessNormalization(enabled bool) error {
	return a.runAudioCommand(func(engine *audio.Engine) error { engine.SetRemoteLoudnessNormalization(enabled); return nil })
}

func (a *App) SetEchoCancellation(enabled bool) error {
	return a.runAudioCommand(func(engine *audio.Engine) error { engine.SetEchoCancellation(enabled); return nil })
}

func (a *App) SetNoiseSuppression(enabled bool) error {
	return a.runAudioCommand(func(engine *audio.Engine) error { engine.SetNoiseSuppression(enabled); return nil })
}

func (a *App) SetNickname(nickname string) error {
	a.waitForStartup()
	nickname, err := peer.NormalizeNickname(nickname)
	if err != nil {
		return err
	}
	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	if a.isShuttingDown() {
		return errors.New("application is shutting down")
	}
	a.stateMu.RLock()
	current := a.nickname
	room := a.room
	audioEngine := a.audioEngine
	a.stateMu.RUnlock()
	if current == nickname {
		return nil
	}
	captureMuted := false
	playbackMuted := false
	if audioEngine != nil {
		status := audioEngine.Status()
		captureMuted = status.CaptureMuted
		playbackMuted = status.PlaybackMuted
	}
	if room != nil && !room.stopping {
		if err := room.client.SetLocalMemberState(nickname, captureMuted, playbackMuted); err != nil {
			return err
		}
	}
	a.stateMu.Lock()
	a.nickname = nickname
	a.stateMu.Unlock()
	a.markStateChanged()
	return nil
}

func (a *App) SetAudioDevices(captureID, playbackID string) error {
	a.waitForStartup()
	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	if a.isShuttingDown() {
		return errors.New("application is shutting down")
	}
	audioEngine, err := a.readyAudioEngine()
	if err != nil {
		return err
	}
	if err = audioEngine.SetDevices(captureID, playbackID); err != nil {
		return err
	}
	a.stateMu.RLock()
	room := a.room
	a.stateMu.RUnlock()
	if room != nil {
		a.reconcileAudioLocked(room)
	}
	a.markStateChanged()
	return nil
}

func (a *App) SyncAudioDevices() error {
	a.waitForStartup()
	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	if a.isShuttingDown() {
		return errors.New("application is shutting down")
	}
	audioEngine, err := a.readyAudioEngine()
	if err != nil {
		if err := a.initializeAudio(); err != nil {
			a.setAudioInitError(err)
			a.markStateChanged()
			return err
		}
		a.setAudioInitError(nil)
		audioEngine, err = a.readyAudioEngine()
		if err != nil {
			return err
		}
		a.stateMu.RLock()
		ctx := a.appContext
		a.stateMu.RUnlock()
		a.startAudioWatcher(ctx)
		a.markStateChanged()
	}
	a.stateMu.RLock()
	room := a.room
	a.stateMu.RUnlock()
	return a.syncAudioDevicesLocked(audioEngine, room)
}

// syncAudioDevicesLocked is shared by UI synchronization and automatic
// recovery. Callers hold commandMu so device rebuilds cannot race room exit.
func (a *App) syncAudioDevicesLocked(audioEngine *audio.Engine, room *roomSession) error {
	if err := audioEngine.RefreshDevices(); err != nil {
		return err
	}
	if room != nil {
		a.reconcileAudioLocked(room)
	}
	return nil
}

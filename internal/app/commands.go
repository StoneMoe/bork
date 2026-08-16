package app

import (
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"

	"bork/internal/audio"
	"bork/internal/globalkey"
	"bork/internal/invite"
	"bork/internal/peer"

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

func (a *App) OfferFile(recipientPeerID string) (string, error) {
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
	a.clearError()
	if a.pushToTalkEnabled {
		if err := a.pushToTalk.Start(a.pushToTalkKey); err != nil {
			a.recordError(err)
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
	a.stopPushToTalkLocked()
	if room != nil {
		if room.screenCaptureID != 0 {
			_ = room.client.StopScreenShare()
			room.screenCaptureID = 0
		}
		room.cancel()
	}
	if audioEngine, err := a.readyAudioEngine(); err == nil {
		audioEngine.Stop()
	}
	a.stateMu.Lock()
	a.lastDiagnostics = emptyDiagnostics()
	a.lastError = nil
	a.stateMu.Unlock()
	a.markStateChanged()
	a.commandMu.Unlock()
	a.stopRoom(room)
	return nil
}

func (a *App) StartScreenShare(codec string, width, height int) (uint32, error) {
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
	if room.screenCaptureID != 0 {
		return 0, errors.New("screen sharing is already active")
	}
	if err := room.client.StartScreenShare(codec, width, height); err != nil {
		return 0, err
	}
	a.nextScreenCaptureID++
	if a.nextScreenCaptureID == 0 {
		a.nextScreenCaptureID++
	}
	room.screenCaptureID = a.nextScreenCaptureID
	return room.screenCaptureID, nil
}

func (a *App) SendScreenVideoChunk(captureID uint32, timestamp uint64, duration uint32, keyFrame bool, bytesBase64 string) (bool, error) {
	a.waitForStartup()
	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	if a.isShuttingDown() {
		return false, errors.New("application is shutting down")
	}
	a.stateMu.RLock()
	room := a.room
	a.stateMu.RUnlock()
	if captureID == 0 || room == nil || room.stopping || room.screenCaptureID != captureID {
		return false, errors.New("screen capture is stale")
	}
	data, err := decodeScreenVideoBase64(bytesBase64)
	if err != nil {
		return false, err
	}
	return room.client.SendScreenVideoChunk(timestamp, duration, keyFrame, data)
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
	if room == nil || room.stopping || room.screenCaptureID == 0 || (captureID != 0 && room.screenCaptureID != captureID) {
		return errors.New("screen capture is stale")
	}
	if err := room.client.StopScreenShare(); err != nil {
		return err
	}
	room.screenCaptureID = 0
	return nil
}

func decodeScreenVideoBase64(value string) ([]byte, error) {
	if len(value) == 0 || len(value) > base64.StdEncoding.EncodedLen(peer.MaxScreenVideoChunkBytes) || strings.ContainsAny(value, "\r\n") {
		return nil, errors.New("screen video base64 length is invalid")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || len(decoded) == 0 || len(decoded) > peer.MaxScreenVideoChunkBytes {
		return nil, errors.New("screen video base64 is invalid")
	}
	return decoded, nil
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
		a.recordError(err)
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
		a.recordError(err)
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

func (a *App) RefreshAudioDevices() error {
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
	}
	if err = audioEngine.RefreshDevices(); err != nil {
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

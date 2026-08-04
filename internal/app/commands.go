package app

import (
	"errors"
	"strings"

	"bork/internal/invite"
	"bork/internal/peer"
)

func (a *App) GetSnapshot() AppSnapshot {
	<-a.startupDone
	return a.snapshot()
}

func (a *App) GetInvite() (string, error) {
	if err := a.waitForStartup(); err != nil {
		return "", err
	}
	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	client, err := a.activeClient()
	if err != nil {
		return "", err
	}
	return client.EncodedInvite(), nil
}

func (a *App) CreateRoom(displayName string) error {
	if err := a.waitForStartup(); err != nil {
		return err
	}
	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	roomInvite, err := invite.New(displayName)
	if err != nil {
		return err
	}
	return a.createRoom(roomInvite)
}

func (a *App) JoinRoom(encodedInvite string) error {
	if err := a.waitForStartup(); err != nil {
		return err
	}
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
	localIdentity := a.localIdentity
	nickname := a.nickname
	audioEngine := a.audioEngine
	hasRoom := a.room != nil
	shuttingDown := a.shuttingDown
	a.stateMu.RUnlock()
	if localIdentity == nil {
		return errors.New("user identity is unavailable")
	}
	if shuttingDown {
		return errors.New("application is shutting down")
	}
	if hasRoom {
		return errors.New("leave the current room before joining another")
	}
	client := peer.NewClient(localIdentity, roomInvite, a.config.NetworkOptions(), a.logger)
	muted := false
	if audioEngine != nil {
		muted = audioEngine.Status().Muted
	}
	if err := client.SetLocalMemberState(nickname, muted); err != nil {
		return err
	}
	if err := a.activateRoom(client); err != nil {
		return err
	}
	a.clearError()
	a.markStateChanged()
	return nil
}

func (a *App) LeaveRoom() error {
	if err := a.waitForStartup(); err != nil {
		return err
	}
	a.commandMu.Lock()
	if a.isShuttingDown() {
		a.commandMu.Unlock()
		return errors.New("application is shutting down")
	}
	room := a.detachActiveRoom()
	if room != nil {
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

func (a *App) SetMuted(muted bool) error {
	if err := a.waitForStartup(); err != nil {
		return err
	}
	a.commandMu.Lock()
	defer a.commandMu.Unlock()
	if a.isShuttingDown() {
		return errors.New("application is shutting down")
	}
	audioEngine, err := a.readyAudioEngine()
	if err != nil {
		return err
	}
	audioEngine.SetMuted(muted)
	a.stateMu.RLock()
	nickname := a.nickname
	room := a.room
	a.stateMu.RUnlock()
	if room != nil && !room.stopping {
		if err := room.client.SetLocalMemberState(nickname, muted); err != nil {
			return err
		}
	}
	a.markStateChanged()
	return nil
}

func (a *App) SetNickname(nickname string) error {
	if err := a.waitForStartup(); err != nil {
		return err
	}
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
	muted := false
	if audioEngine != nil {
		muted = audioEngine.Status().Muted
	}
	if room != nil && !room.stopping {
		if err := room.client.SetLocalMemberState(nickname, muted); err != nil {
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
	if err := a.waitForStartup(); err != nil {
		return err
	}
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
	if err := a.waitForStartup(); err != nil {
		return err
	}
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

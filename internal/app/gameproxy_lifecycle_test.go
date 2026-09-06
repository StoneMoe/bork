//go:build game_proxy

package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"bork/internal/config"
	"bork/internal/gameproxy"
)

func TestGameProxyWatcher_marks_state_changes_and_cancels(t *testing.T) {
	manager := newFakeGameProxyManager()
	application := NewApp(config.AppConfig{}, nil)
	application.gameProxyManager = manager
	application.startGameProxyWatcher(t.Context())

	manager.changes <- struct{}{}
	receiveTestSignal(t, application.statePending)
	application.stopGameProxyWatcher()
	manager.changes <- struct{}{}

	select {
	case <-application.statePending:
		t.Fatal("cancelled watcher published a state change")
	default:
	}
}

func TestShutdown_stops_manager_without_locks_during_blocked_start(t *testing.T) {
	manager := newFakeGameProxyManager()
	startEntered := make(chan struct{})
	startDone := make(chan struct{})
	manager.startFunc = func(ctx context.Context, _ gameproxy.StartInput) error {
		close(startEntered)
		<-ctx.Done()
		close(startDone)
		return context.Canceled
	}
	stopLocksFree := make(chan bool, 1)
	application := startedGameProxyTestApp(config.AppConfig{GameProxy: validConfigGameProxy("/games")})
	application.gameProxyManager = manager
	manager.stopFunc = func() {
		commandUnlocked := application.commandMu.TryLock()
		if commandUnlocked {
			application.commandMu.Unlock()
		}
		stateUnlocked := application.stateMu.TryLock()
		if stateUnlocked {
			application.stateMu.Unlock()
		}
		stopLocksFree <- commandUnlocked && stateUnlocked
		<-startDone
	}
	if err := application.StartGameProxy(); err != nil {
		t.Fatal(err)
	}
	receiveTestSignal(t, startEntered)
	shutdownDone := make(chan struct{})
	go func() {
		application.shutdown(context.Background())
		close(shutdownDone)
	}()

	if locksFree := receiveTestValue(t, stopLocksFree); !locksFree {
		t.Fatal("shutdown called manager Stop while holding an App lock")
	}
	receiveTestSignal(t, shutdownDone)
}

func TestShutdown_cancels_context_used_by_async_start_before_manager_stop(t *testing.T) {
	parent, cancelParent := context.WithCancel(t.Context())
	defer cancelParent()
	manager := newFakeGameProxyManager()
	startContext := make(chan context.Context, 1)
	startEntered := make(chan struct{})
	startDone := make(chan struct{})
	manager.startFunc = func(ctx context.Context, _ gameproxy.StartInput) error {
		startContext <- ctx
		close(startEntered)
		<-ctx.Done()
		close(startDone)
		return ctx.Err()
	}
	stopSawCancellation := make(chan bool, 1)
	manager.stopFunc = func() {
		runCtx := receiveTestValue(t, startContext)
		select {
		case <-runCtx.Done():
			stopSawCancellation <- true
		default:
			stopSawCancellation <- false
		}
		<-startDone
	}
	application := startedGameProxyTestAppWithContext(
		config.AppConfig{GameProxy: validConfigGameProxy("/games")},
		parent,
	)
	application.gameProxyManager = manager
	if err := application.StartGameProxy(); err != nil {
		t.Fatal(err)
	}
	receiveTestSignal(t, startEntered)
	shutdownDone := make(chan struct{})
	go func() {
		application.shutdown(context.Background())
		close(shutdownDone)
	}()

	if cancelled := receiveTestValue(t, stopSawCancellation); !cancelled {
		t.Fatal("manager Stop ran before the async start context was cancelled")
	}
	receiveTestSignal(t, shutdownDone)
	select {
	case <-startDone:
	default:
		t.Fatal("async start remained live after shutdown")
	}
}

func TestStopGameProxy_drains_unaccepted_start_before_stopping_and_allows_restart(t *testing.T) {
	manager := newFakeGameProxyManager()
	startContext := make(chan context.Context, 1)
	manager.startFunc = func(ctx context.Context, _ gameproxy.StartInput) error {
		startContext <- ctx
		<-ctx.Done()
		// Model acceptance racing cancellation, before Start returns to the app.
		manager.setStatus(gameproxy.Status{Supported: true, State: gameproxy.StateRunning})
		return ctx.Err()
	}
	manager.stopFunc = func() {
		manager.setStatus(gameproxy.Status{Supported: true, State: gameproxy.StateInactive})
	}
	application := startedGameProxyTestAppWithContext(config.AppConfig{GameProxy: validConfigGameProxy("/games")}, t.Context())
	application.gameProxyManager = manager
	for attempt := 0; attempt < 2; attempt++ {
		if err := application.StartGameProxy(); err != nil {
			t.Fatal(err)
		}
		ctx := receiveTestValue(t, startContext)
		if ctx.Err() != nil {
			t.Fatal("start inherited a previously cancelled context")
		}
		if err := application.StartGameProxy(); !errors.Is(err, gameproxy.ErrActive) {
			t.Fatalf("duplicate pending start = %v", err)
		}
		if err := application.SaveGameProxyConfig(validGameProxyConfigInput("/new")); err == nil || !strings.Contains(err.Error(), "is starting") {
			t.Fatalf("save before manager accepted the start = %v", err)
		}
		done := make(chan struct{})
		go func() {
			application.StopGameProxy()
			close(done)
		}()
		receiveTestSignal(t, done)
		if manager.Status().State != gameproxy.StateInactive || ctx.Err() == nil {
			t.Fatal("Stop returned without cancelling and draining the pending start")
		}
	}
}

func TestSaveGameProxyConfig_keeps_shared_commands_and_teardown_responsive(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		name := "stop"
		if shutdown {
			name = "shutdown"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			manager := newFakeGameProxyManager()
			manager.setStatus(gameproxy.Status{Supported: true, State: gameproxy.StateRunning, Directories: []string{"/old"}})
			entered := make(chan struct{})
			manager.updateDirectoriesFunc = func(ctx context.Context, _ []string) error {
				close(entered)
				<-ctx.Done()
				return ctx.Err()
			}
			application := startedGameProxyTestAppWithContext(config.AppConfig{
				FilePath: filepath.Join(t.TempDir(), "config.yml"), GameProxy: validConfigGameProxy("/old"),
			}, ctx)
			application.gameProxyManager = manager
			saved := make(chan error, 1)
			go func() { saved <- application.SaveGameProxyConfig(validGameProxyConfigInput("/new")) }()
			receiveTestSignal(t, entered)

			commandsDone := make(chan struct{})
			go func() {
				application.applyPushToTalk(false)
				_ = application.SetCaptureMuted(true)
				_ = application.GetSnapshot()
				close(commandsDone)
			}()
			receiveTestSignal(t, commandsDone)
			stopped := make(chan struct{})
			go func() {
				if shutdown {
					application.shutdown(context.Background())
				} else {
					application.StopGameProxy()
				}
				close(stopped)
			}()
			receiveTestSignal(t, stopped)
			if err := receiveTestValue(t, saved); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled save = %v", err)
			}
		})
	}
}

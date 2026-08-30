package app

import (
	"context"
	"sync"
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
	startGate := make(chan struct{})
	startDone := make(chan struct{})
	manager.startFunc = func(context.Context, gameproxy.StartInput) error {
		close(startEntered)
		<-startGate
		close(startDone)
		return context.Canceled
	}
	stopLocksFree := make(chan bool, 1)
	var stopOnce sync.Once
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
		stopOnce.Do(func() { close(startGate) })
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
	startDone := make(chan struct{})
	manager.startFunc = func(ctx context.Context, _ gameproxy.StartInput) error {
		startContext <- ctx
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

package audio

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"bork/internal/media"
)

func TestUnexpectedDeviceStopDoesNotWaitForItsMonitor(t *testing.T) {
	engine, run, stopped := testRunningEngine()
	stopped <- struct{}{}
	select {
	case <-run.monitorDone:
	case <-time.After(time.Second):
		t.Fatal("unexpected-stop monitor deadlocked")
	}
	if engine.Status().Running {
		t.Fatal("engine remained running after device stop")
	}
}

func TestExplicitStopRacingDeviceStopReturns(t *testing.T) {
	engine, _, stopped := testRunningEngine()
	done := make(chan struct{})
	go func() {
		engine.Stop()
		close(done)
	}()
	stopped <- struct{}{}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("explicit Stop deadlocked with device-stop monitor")
	}
}

func testRunningEngine() (*Engine, *engineRun, chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	run := &engineRun{cancel: cancel, port: media.NewFlow(), monitorDone: make(chan struct{})}
	engine := &Engine{
		logger:        slog.Default(),
		run:           run,
		state:         Status{Running: true},
		statusChanges: make(chan struct{}, 1),
	}
	stopped := make(chan struct{}, 1)
	go engine.watchDeviceStop(ctx, run, stopped)
	return engine, run, stopped
}

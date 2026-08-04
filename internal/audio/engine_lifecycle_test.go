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
	engine.mu.Lock()
	engine.state.Speaking = true
	engine.state.SpeakingPeerIDs = []string{"peer"}
	engine.mu.Unlock()
	stopped <- struct{}{}
	select {
	case <-run.monitorDone:
	case <-time.After(time.Second):
		t.Fatal("unexpected-stop monitor deadlocked")
	}
	if state := engine.Status(); state.Running || state.Speaking || len(state.SpeakingPeerIDs) != 0 {
		t.Fatalf("audio activity remained after device stop: %#v", state)
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

func TestMuteAndStopClearSpeaking(t *testing.T) {
	engine, _, _ := testRunningEngine()
	engine.mu.Lock()
	engine.state.Speaking = true
	engine.state.SpeakingPeerIDs = []string{"peer"}
	engine.mu.Unlock()

	engine.SetMuted(true)
	if state := engine.Status(); !state.Muted || state.Speaking || len(state.SpeakingPeerIDs) != 1 {
		t.Fatalf("audio state after mute = %#v", state)
	}
	engine.SetMuted(false)
	engine.mu.Lock()
	engine.state.Speaking = true
	engine.mu.Unlock()
	engine.Stop()
	if state := engine.Status(); state.Running || state.Speaking || state.SpeakingPeerIDs == nil || len(state.SpeakingPeerIDs) != 0 {
		t.Fatalf("audio state after stop = %#v", state)
	}
}

func TestStaleCaptureGenerationCannotReactivateSpeaking(t *testing.T) {
	run := &engineRun{}
	run.active.Store(true)
	run.captureGeneration.Store(2)
	engine := &Engine{state: Status{SpeakingPeerIDs: []string{}}, statusChanges: make(chan struct{}, 1)}

	engine.setLocalSpeaking(run, 1, true)
	if engine.Status().Speaking {
		t.Fatal("stale capture generation reactivated speaking")
	}
	select {
	case <-engine.StatusChanges():
		t.Fatal("stale capture generation published a status change")
	default:
	}

	engine.setLocalSpeaking(run, 2, true)
	if !engine.Status().Speaking {
		t.Fatal("current capture generation did not activate speaking")
	}
	<-engine.StatusChanges()
	engine.setLocalSpeaking(run, 2, true)
	select {
	case <-engine.StatusChanges():
		t.Fatal("unchanged local speaking state was published")
	default:
	}
}

func TestSpeakingPeerIDsSnapshotAndTransitionPublishing(t *testing.T) {
	run := &engineRun{}
	run.active.Store(true)
	engine := &Engine{state: Status{SpeakingPeerIDs: []string{}}, statusChanges: make(chan struct{}, 1)}

	engine.setSpeakingPeerIDs(run, []string{"peer-a", "peer-b"})
	status := engine.Status()
	status.SpeakingPeerIDs[0] = "changed"
	if got := engine.Status().SpeakingPeerIDs[0]; got != "peer-a" {
		t.Fatalf("status snapshot mutated engine state: %q", got)
	}
	<-engine.StatusChanges()
	engine.setSpeakingPeerIDs(run, []string{"peer-a", "peer-b"})
	select {
	case <-engine.StatusChanges():
		t.Fatal("unchanged speaking peer IDs were published")
	default:
	}
	if cloneStatus(Status{}).SpeakingPeerIDs == nil {
		t.Fatal("empty speaking peer IDs snapshot is nil")
	}
}

func testRunningEngine() (*Engine, *engineRun, chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	run := &engineRun{cancel: cancel, port: media.NewFlow(), monitorDone: make(chan struct{})}
	run.active.Store(true)
	engine := &Engine{
		logger:        slog.Default(),
		run:           run,
		state:         Status{Running: true, SpeakingPeerIDs: []string{}},
		statusChanges: make(chan struct{}, 1),
	}
	stopped := make(chan struct{}, 1)
	go engine.watchDeviceStop(ctx, run, stopped)
	return engine, run, stopped
}

package gameproxy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"bork/internal/gameproxy/iwan"
)

func TestManager_reconnects_without_admitting_stale_generation(t *testing.T) {
	// Given
	log := &eventLog{}
	bridge := newFakeBridge(log)
	supervisor := newFakeSupervisor(log, iwan.Status{State: iwan.StateReady, Generation: 1})
	manager := newTestManager(log, supervisor, &fakeBridgeFactory{log: log, supported: true, bridge: bridge})
	if err := manager.Start(context.Background(), validStartInput()); err != nil {
		t.Fatal(err)
	}
	changes := manager.Changes()

	// When
	supervisor.publish(iwan.Status{State: iwan.StateRetrying, Generation: 1, Err: errors.New("lost")})
	waitManagerState(t, manager, changes, StateReconnecting)
	supervisor.publish(iwan.Status{State: iwan.StateReady, Generation: 2})
	waitManagerState(t, manager, changes, StateRunning)

	// Then
	if status := manager.Status(); status.Generation != 2 || status.Error != "" {
		t.Fatalf("reconnected status = %#v", status)
	}
	manager.Stop()
}

func TestManager_terminal_iwan_failure_closes_bridge_before_stopping_iwan(t *testing.T) {
	// Given
	log := &eventLog{}
	bridge := newFakeBridge(log)
	supervisor := newFakeSupervisor(log, iwan.Status{State: iwan.StateReady, Generation: 3})
	manager := newTestManager(log, supervisor, &fakeBridgeFactory{log: log, supported: true, bridge: bridge})
	if err := manager.Start(context.Background(), validStartInput()); err != nil {
		t.Fatal(err)
	}
	changes := manager.Changes()

	// When
	failure := errors.New("authentication rejected")
	supervisor.publish(iwan.Status{State: iwan.StateFailed, Generation: 3, Err: failure})
	waitManagerState(t, manager, changes, StateFailed)

	// Then
	events := log.snapshot()
	if !ordered(events, "bridge-close", "iwan-stop") {
		t.Fatalf("failure cleanup order = %q", events)
	}
	if status := manager.Status(); status.Error == "" {
		t.Fatalf("failed status = %#v", status)
	}
}

func TestManager_bridge_failure_stops_iwan_and_publishes_failed(t *testing.T) {
	// Given
	log := &eventLog{}
	bridge := newFakeBridge(log)
	supervisor := newFakeSupervisor(log, iwan.Status{State: iwan.StateReady, Generation: 4})
	manager := newTestManager(log, supervisor, &fakeBridgeFactory{log: log, supported: true, bridge: bridge})
	if err := manager.Start(context.Background(), validStartInput()); err != nil {
		t.Fatal(err)
	}
	changes := manager.Changes()

	// When
	bridge.failures <- errors.New("native loop failed")
	waitManagerState(t, manager, changes, StateFailed)

	// Then
	if !ordered(log.snapshot(), "bridge-close", "iwan-stop") {
		t.Fatalf("failure cleanup order = %q", log.snapshot())
	}
}

func TestManager_Stop_during_start_is_idempotent(t *testing.T) {
	// Given
	log := &eventLog{}
	bridge := newFakeBridge(log)
	supervisor := newFakeSupervisor(log, iwan.Status{State: iwan.StateConnecting})
	supervisor.waitGate = make(chan struct{})
	manager := newTestManager(log, supervisor, &fakeBridgeFactory{log: log, supported: true, bridge: bridge})
	startResult := make(chan error, 1)
	go func() { startResult <- manager.Start(context.Background(), validStartInput()) }()
	<-supervisor.waiting

	// When
	manager.Stop()
	manager.Stop()

	// Then
	if err := <-startResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start error = %v, want context canceled", err)
	}
	if status := manager.Status(); status.State != StateInactive {
		t.Fatalf("status after Stop = %#v", status)
	}
	if supervisor.stopCalls() != 1 {
		t.Fatalf("iwan Stop calls = %d, want 1", supervisor.stopCalls())
	}
}

func TestManager_duplicate_start_does_not_construct_second_run(t *testing.T) {
	// Given
	log := &eventLog{}
	supervisor := newFakeSupervisor(log, iwan.Status{State: iwan.StateConnecting})
	supervisor.waitGate = make(chan struct{})
	manager := newTestManager(log, supervisor, &fakeBridgeFactory{log: log, supported: true, bridge: newFakeBridge(log)})
	firstResult := make(chan error, 1)
	go func() { firstResult <- manager.Start(context.Background(), validStartInput()) }()
	<-supervisor.waiting

	// When
	err := manager.Start(context.Background(), validStartInput())

	// Then
	if !errors.Is(err, ErrActive) {
		t.Fatalf("duplicate Start error = %v, want ErrActive", err)
	}
	manager.Stop()
	<-firstResult
	newCount := 0
	for _, event := range log.snapshot() {
		if event == "iwan-new" {
			newCount++
		}
	}
	if newCount != 1 {
		t.Fatalf("iwan constructions = %d, want 1", newCount)
	}
}

func TestManager_parent_context_cancellation_stops_active_run(t *testing.T) {
	// Given
	ctx, cancel := context.WithCancel(context.Background())
	log := &eventLog{}
	bridge := newFakeBridge(log)
	supervisor := newFakeSupervisor(log, iwan.Status{State: iwan.StateReady, Generation: 8})
	manager := newTestManager(log, supervisor, &fakeBridgeFactory{log: log, supported: true, bridge: bridge})
	if err := manager.Start(ctx, validStartInput()); err != nil {
		t.Fatal(err)
	}
	changes := manager.Changes()

	// When
	cancel()
	waitManagerState(t, manager, changes, StateInactive)

	// Then
	if !ordered(log.snapshot(), "bridge-close", "iwan-stop") {
		t.Fatalf("cancellation cleanup order = %q", log.snapshot())
	}
}

func TestManager_concurrent_Stop_waits_for_one_cleanup(t *testing.T) {
	// Given
	log := &eventLog{}
	bridge := newFakeBridge(log)
	supervisor := newFakeSupervisor(log, iwan.Status{State: iwan.StateReady, Generation: 9})
	manager := newTestManager(log, supervisor, &fakeBridgeFactory{log: log, supported: true, bridge: bridge})
	if err := manager.Start(context.Background(), validStartInput()); err != nil {
		t.Fatal(err)
	}

	// When
	var stops sync.WaitGroup
	for range 8 {
		stops.Add(1)
		go func() {
			defer stops.Done()
			manager.Stop()
		}()
	}
	stops.Wait()

	// Then
	if supervisor.stopCalls() != 1 {
		t.Fatalf("iwan Stop calls = %d, want 1", supervisor.stopCalls())
	}
	if status := manager.Status(); status.State != StateInactive {
		t.Fatalf("status after concurrent Stop = %#v", status)
	}
}

func TestManager_ignores_stale_iwan_update_after_Stop(t *testing.T) {
	// Given
	log := &eventLog{}
	bridge := newFakeBridge(log)
	supervisor := newFakeSupervisor(log, iwan.Status{State: iwan.StateReady, Generation: 10})
	manager := newTestManager(log, supervisor, &fakeBridgeFactory{log: log, supported: true, bridge: bridge})
	if err := manager.Start(context.Background(), validStartInput()); err != nil {
		t.Fatal(err)
	}
	manager.Stop()

	// When
	supervisor.publish(iwan.Status{State: iwan.StateFailed, Generation: 10, Err: errors.New("late failure")})

	// Then
	if status := manager.Status(); status.State != StateInactive || status.Error != "" {
		t.Fatalf("status after stale update = %#v", status)
	}
}

func TestManager_Changes_coalesces_notifications(t *testing.T) {
	// Given
	log := &eventLog{}
	manager := newManager(managerDependencies{bridge: &fakeBridgeFactory{log: log, supported: true}})
	changes := manager.Changes()

	// When
	manager.publish(Status{Supported: true, State: StateStarting})
	manager.publish(Status{Supported: true, State: StateFailed, Error: "failed"})

	// Then
	select {
	case <-changes:
	default:
		t.Fatal("Changes did not report a status update")
	}
	select {
	case <-changes:
		t.Fatal("Changes did not coalesce updates")
	default:
	}
}

func waitManagerState(t *testing.T, manager *Manager, changes <-chan struct{}, want State) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for manager.Status().State != want {
		select {
		case <-changes:
		case <-ctx.Done():
			t.Fatalf("waiting for state %q: %v", want, ctx.Err())
		}
	}
}

func ordered(events []string, first, second string) bool {
	firstIndex := -1
	for index, event := range events {
		if event == first && firstIndex < 0 {
			firstIndex = index
		}
		if event == second {
			return firstIndex >= 0 && firstIndex < index
		}
	}
	return false
}

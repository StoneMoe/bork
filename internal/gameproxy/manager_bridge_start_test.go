package gameproxy

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"bork/internal/gameproxy/intercept"
	"bork/internal/gameproxy/iwan"
)

func TestManager_Start_does_not_publish_running_before_bridge_ready(t *testing.T) {
	log := &eventLog{}
	bridge := newFakeBridge(log)
	bridge.startGate = make(chan struct{})
	supervisor := newFakeSupervisor(log, iwan.Status{State: iwan.StateReady, Generation: 11})
	manager := newTestManager(log, supervisor, &fakeBridgeFactory{log: log, supported: true, bridge: bridge})
	startResult := make(chan error, 1)
	go func() { startResult <- manager.Start(context.Background(), validStartInput()) }()
	<-bridge.started

	if status := manager.Status(); status.State != StateStarting {
		t.Fatalf("status while bridge Start is blocked = %#v, want starting", status)
	}
	select {
	case err := <-startResult:
		t.Fatalf("Manager.Start returned before bridge readiness: %v", err)
	default:
	}
	close(bridge.startGate)
	if err := <-startResult; err != nil {
		t.Fatal(err)
	}
	manager.Stop()
}

func TestManager_Start_sets_generation_ready_before_bridge_start(t *testing.T) {
	log := &eventLog{}
	bridge := newFakeBridge(log)
	supervisor := newFakeSupervisor(log, iwan.Status{State: iwan.StateReady, Generation: 12})
	manager := newTestManager(log, supervisor, &fakeBridgeFactory{log: log, supported: true, bridge: bridge})

	err := manager.Start(context.Background(), validStartInput())

	if err != nil {
		t.Fatal(err)
	}
	want := intercept.GenerationState{Generation: 12, Ready: true}
	if got := bridge.generationState(); got != want {
		t.Fatalf("generation during bridge Start = %+v, want %+v", got, want)
	}
	manager.Stop()
}

func TestManager_Start_bridge_initialization_failure_closes_bridge_before_iwan(t *testing.T) {
	bridgeFailure := errors.New("install redirect rules")
	log := &eventLog{}
	bridge := newFakeBridge(log)
	bridge.startErr = bridgeFailure
	supervisor := newFakeSupervisor(log, iwan.Status{State: iwan.StateReady, Generation: 13})
	manager := newTestManager(log, supervisor, &fakeBridgeFactory{log: log, supported: true, bridge: bridge})

	err := manager.Start(context.Background(), validStartInput())

	if !errors.Is(err, bridgeFailure) {
		t.Fatalf("Manager.Start error = %v, want bridge initialization failure", err)
	}
	wantOrder := []string{
		"scan", "bridge-available", "iwan-new", "iwan-start", "iwan-wait",
		"bridge-new", "bridge-start", "bridge-close", "iwan-stop",
	}
	if got := log.snapshot(); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("start failure order = %q, want %q", got, wantOrder)
	}
	if status := manager.Status(); status.State != StateFailed {
		t.Fatalf("status after bridge Start failure = %#v, want failed", status)
	}
}

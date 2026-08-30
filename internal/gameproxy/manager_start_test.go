package gameproxy

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"testing"

	"bork/internal/gameproxy/iwan"
)

func TestManager_Status_reports_platform_support(t *testing.T) {
	// Given
	log := &eventLog{}
	unsupported := newManager(managerDependencies{bridge: &fakeBridgeFactory{log: log}})
	supported := newManager(managerDependencies{bridge: &fakeBridgeFactory{log: log, supported: true}})

	// When
	unsupportedStatus := unsupported.Status()
	supportedStatus := supported.Status()

	// Then
	if unsupportedStatus.Supported || unsupportedStatus.State != StateUnsupported {
		t.Fatalf("unsupported status = %#v", unsupportedStatus)
	}
	if !supportedStatus.Supported || supportedStatus.State != StateInactive {
		t.Fatalf("supported status = %#v", supportedStatus)
	}
}

func TestManager_Start_reaches_running_only_after_dependencies_ready(t *testing.T) {
	// Given
	log := &eventLog{}
	bridge := newFakeBridge(log)
	bridgeFactory := &fakeBridgeFactory{log: log, supported: true, bridge: bridge}
	supervisor := newFakeSupervisor(log, iwan.Status{State: iwan.StateReady, Generation: 7})
	manager := newTestManager(log, supervisor, bridgeFactory)

	// When
	err := manager.Start(context.Background(), validStartInput())

	// Then
	if err != nil {
		t.Fatal(err)
	}
	<-bridge.started
	wantOrder := []string{"scan", "bridge-available", "iwan-new", "iwan-start", "iwan-wait", "bridge-new", "bridge-start"}
	if got := log.snapshot(); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("start order = %q, want %q", got, wantOrder)
	}
	status := manager.Status()
	if status.State != StateRunning || status.Generation != 7 || status.ExecutableCount != 2 || status.Directory != "games" || status.Error != "" {
		t.Fatalf("running status = %#v", status)
	}
	manager.Stop()
}

func TestManager_Start_rejects_unsupported_platform(t *testing.T) {
	// Given
	log := &eventLog{}
	manager := newManager(managerDependencies{bridge: &fakeBridgeFactory{log: log}})

	// When
	err := manager.Start(context.Background(), validStartInput())

	// Then
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Start error = %v, want ErrUnsupported", err)
	}
	if got := log.snapshot(); len(got) != 0 {
		t.Fatalf("events = %q, want none", got)
	}
}

func TestManager_Start_rejects_empty_rules_before_bridge_availability(t *testing.T) {
	// Given
	log := &eventLog{}
	bridgeFactory := &fakeBridgeFactory{log: log, supported: true}
	manager := newManager(managerDependencies{
		bridge: bridgeFactory,
		scanRules: func(string) (ruleSet, error) {
			log.add("scan")
			return ruleSet{matcher: fakeMatcher{}}, nil
		},
	})

	// When
	err := manager.Start(context.Background(), validStartInput())

	// Then
	if !errors.Is(err, ErrNoExecutables) {
		t.Fatalf("Start error = %v, want ErrNoExecutables", err)
	}
	if got := log.snapshot(); !reflect.DeepEqual(got, []string{"scan"}) {
		t.Fatalf("events = %q, want scan only", got)
	}
	if status := manager.Status(); status.State != StateFailed || status.ExecutableCount != 0 {
		t.Fatalf("failed status = %#v", status)
	}
}

func TestManager_Start_fails_closed_when_bridge_unavailable(t *testing.T) {
	// Given
	availabilityFailure := errors.New("elevation denied")
	log := &eventLog{}
	bridgeFactory := &fakeBridgeFactory{log: log, supported: true, availableErr: availabilityFailure}
	supervisor := newFakeSupervisor(log, iwan.Status{State: iwan.StateReady, Generation: 1})
	manager := newTestManager(log, supervisor, bridgeFactory)

	// When
	err := manager.Start(context.Background(), validStartInput())

	// Then
	if !errors.Is(err, availabilityFailure) {
		t.Fatalf("Start error = %v, want availability failure", err)
	}
	wantOrder := []string{"scan", "bridge-available"}
	if got := log.snapshot(); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("events = %q, want %q", got, wantOrder)
	}
	if status := manager.Status(); status.State != StateFailed || status.Error == "" {
		t.Fatalf("failed status = %#v", status)
	}
}

func TestManager_Start_stops_iwan_when_readiness_fails(t *testing.T) {
	// Given
	failure := errors.New("authentication failed")
	log := &eventLog{}
	supervisor := newFakeSupervisor(log, iwan.Status{State: iwan.StateFailed, Err: failure})
	supervisor.waitErr = failure
	manager := newTestManager(log, supervisor, &fakeBridgeFactory{log: log, supported: true})

	// When
	err := manager.Start(context.Background(), validStartInput())

	// Then
	if !errors.Is(err, failure) {
		t.Fatalf("Start error = %v, want readiness failure", err)
	}
	if supervisor.stopCalls() != 1 {
		t.Fatalf("iwan Stop calls = %d, want 1", supervisor.stopCalls())
	}
	if status := manager.Status(); status.State != StateFailed {
		t.Fatalf("failed status = %#v", status)
	}
}

func TestManager_Start_validates_before_scanning(t *testing.T) {
	// Given
	log := &eventLog{}
	manager := newManager(managerDependencies{
		bridge: &fakeBridgeFactory{log: log, supported: true},
		scanRules: func(string) (ruleSet, error) {
			log.add("scan")
			return ruleSet{}, nil
		},
	})
	input := validStartInput()
	input.DNS = netip.MustParseAddr("2001:db8::1")

	// When
	err := manager.Start(context.Background(), input)

	// Then
	if !errors.Is(err, ErrInvalidStartInput) {
		t.Fatalf("Start error = %v, want ErrInvalidStartInput", err)
	}
	if got := log.snapshot(); len(got) != 0 {
		t.Fatalf("events = %q, want none", got)
	}
}

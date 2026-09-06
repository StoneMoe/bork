//go:build game_proxy

package iwan

import (
	"errors"
	"testing"
)

func TestSupervisor_Changes_coalesces_status_notifications(t *testing.T) {
	// Given
	supervisor, err := newSupervisor(Options{Node: Node{
		Server: "127.0.0.1", Username: "user", Password: "secret",
	}}, testTimings())
	if err != nil {
		t.Fatal(err)
	}
	changes := supervisor.Changes()

	// When
	supervisor.mu.Lock()
	supervisor.publishLocked(Status{State: StateConnecting})
	supervisor.publishLocked(Status{State: StateAuthenticating, Generation: 1})
	supervisor.mu.Unlock()

	// Then
	select {
	case <-changes:
	default:
		t.Fatal("Changes did not report a status update")
	}
	select {
	case <-changes:
		t.Fatal("Changes did not coalesce status updates")
	default:
	}
}

func TestSupervisor_retry_publishes_exact_generation_end_snapshot(t *testing.T) {
	supervisor, err := newSupervisor(Options{Node: Node{
		Server: "127.0.0.1", Username: "user", Password: "secret",
	}}, testTimings())
	if err != nil {
		t.Fatal(err)
	}
	dataPath := &dataPathCounters{}
	dataPath.outboundStackPackets.Store(3)
	supervisor.mu.Lock()
	supervisor.desired = true
	supervisor.status = Status{State: StateReady, Generation: 7}
	supervisor.mu.Unlock()

	supervisor.retry(7, dataPath, errors.New("lost"))
	status, events, dropped := supervisor.DrainDataPathEvents()

	if dropped != 0 || len(events) != 1 || events[0].Phase != DataPathEventEnd ||
		events[0].Status.Generation != 7 || events[0].Status.DataPath.OutboundStackPackets != 3 {
		t.Fatalf("data path events = %#v, dropped %d", events, dropped)
	}
	if status.State != StateRetrying || status.Generation != 7 {
		t.Fatalf("retry status = %#v", status)
	}
}

func TestSupervisor_DrainDataPathEvents_bounds_queue_and_reports_drops(t *testing.T) {
	supervisor, err := newSupervisor(Options{Node: Node{
		Server: "127.0.0.1", Username: "user", Password: "secret",
	}}, testTimings())
	if err != nil {
		t.Fatal(err)
	}
	supervisor.mu.Lock()
	for generation := uint64(1); generation <= maxDataPathEvents+1; generation++ {
		supervisor.appendDataPathEventLocked(DataPathEvent{
			Phase: DataPathEventStart, Status: Status{State: StateReady, Generation: generation},
		})
	}
	supervisor.mu.Unlock()

	_, events, dropped := supervisor.DrainDataPathEvents()

	if len(events) != maxDataPathEvents || dropped != 1 || events[0].Status.Generation != 2 {
		t.Fatalf("bounded data path events = %d entries starting at %d, dropped %d", len(events), events[0].Status.Generation, dropped)
	}
	_, remaining, remainingDropped := supervisor.DrainDataPathEvents()
	if len(remaining) != 0 || remainingDropped != 0 {
		t.Fatalf("remaining data path events = %#v, dropped %d", remaining, remainingDropped)
	}
}

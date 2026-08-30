package iwan

import "testing"

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

package globalkey

import (
	"errors"
	"testing"
)

func TestListenerKeepsLatestKeyState(t *testing.T) {
	listener := New()
	listener.setPressed(true)
	listener.setPressed(true)
	listener.setPressed(false)
	if listener.Pressed() {
		t.Fatal("listener retained a released key")
	}
	select {
	case <-listener.Changes():
	default:
		t.Fatal("listener did not publish a state change")
	}
}

func TestValidCode(t *testing.T) {
	for _, code := range []string{DefaultCode, "KeyV", "Digit7", "F12", "ArrowUp", "NumpadEnter"} {
		if !ValidCode(code) {
			t.Fatalf("expected %q to be supported", code)
		}
	}
	for _, code := range []string{"", "Unidentified", "ShiftLeft", "CapsLock", "F13", "Keyé"} {
		if ValidCode(code) {
			t.Fatalf("expected %q to be rejected", code)
		}
	}
}

func TestListenerKeepsCurrentError(t *testing.T) {
	listener := New()
	listener.setError(errors.New("old listener failed"))
	listener.Stop()
	listener.setError(errors.New("current listener failed"))
	event := <-listener.Errors()
	if !listener.IsCurrent(event) || event.Error() != "current listener failed" {
		t.Fatal("listener retained an error from its previous run")
	}
}

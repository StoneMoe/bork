package globalkey

import (
	"strconv"
	"strings"
	"sync/atomic"
)

const DefaultCode = "Backquote"

// Listener reports the latest state of one physical keyboard key. Platform
// callbacks only update this state; the application performs audio work on its
// own goroutine.
type Listener struct {
	pressed    atomic.Bool
	generation atomic.Uint64
	changes    chan struct{}
	errors     chan ListenerError
	platformListener
}

func New() *Listener {
	listener := &Listener{changes: make(chan struct{}, 1), errors: make(chan ListenerError, 1)}
	listener.initPlatform(listener)
	return listener
}

func (l *Listener) Start(code string) error {
	// Fully stop the old source before clearing its state. A callback already
	// in flight must not reopen the microphone after the key has changed.
	l.stopPlatform()
	l.setPressed(false)
	l.generation.Add(1)
	return l.startPlatform(code)
}

func (l *Listener) Stop() {
	l.stopPlatform()
	l.setPressed(false)
	l.generation.Add(1)
}

func (l *Listener) Pressed() bool {
	return l.pressed.Load()
}

func (l *Listener) Changes() <-chan struct{} {
	return l.changes
}

func (l *Listener) Errors() <-chan ListenerError {
	return l.errors
}

// ListenerError identifies the listener run which produced an asynchronous
// platform error, so a late error cannot affect a later room.
type ListenerError struct {
	Err        error
	generation uint64
}

func (e ListenerError) Error() string {
	return e.Err.Error()
}

func (l *Listener) IsCurrent(event ListenerError) bool {
	return event.generation == l.generation.Load()
}

func (l *Listener) setPressed(pressed bool) {
	if l.pressed.Swap(pressed) == pressed {
		return
	}
	select {
	case l.changes <- struct{}{}:
	default:
	}
}

func (l *Listener) setError(err error) {
	if err == nil {
		return
	}
	event := ListenerError{Err: err, generation: l.generation.Load()}
	// Keep the newest failure. A stale error must not occupy the one-item
	// channel and hide a failure from the current room.
	for {
		select {
		case l.errors <- event:
			return
		case <-l.errors:
		}
	}
}

// ValidCode accepts the physical KeyboardEvent.code values supported by every
// platform implementation. Modifier-only and lock keys are intentionally not
// valid push-to-talk bindings.
func ValidCode(code string) bool {
	return validLetterOrDigit(code) || validFunctionKey(code) || validNamedCode(code)
}

func validLetterOrDigit(code string) bool {
	if len(code) == 4 && strings.HasPrefix(code, "Key") {
		return code[3] >= 'A' && code[3] <= 'Z'
	}
	return len(code) == 6 && strings.HasPrefix(code, "Digit") && code[5] >= '0' && code[5] <= '9'
}

func validFunctionKey(code string) bool {
	if len(code) < 2 || code[0] != 'F' {
		return false
	}
	value, err := strconv.Atoi(code[1:])
	return err == nil && value >= 1 && value <= 12
}

func validNamedCode(code string) bool {
	switch code {
	case "Backquote", "Minus", "Equal", "Backspace", "Tab", "BracketLeft", "BracketRight", "Backslash",
		"Semicolon", "Quote", "Enter", "Comma", "Period", "Slash", "Space", "IntlBackslash",
		"Insert", "Home", "PageUp", "Delete", "End", "PageDown", "ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown",
		"NumpadDivide", "NumpadMultiply", "NumpadSubtract", "NumpadAdd", "NumpadEnter", "NumpadDecimal",
		"Numpad0", "Numpad1", "Numpad2", "Numpad3", "Numpad4", "Numpad5", "Numpad6", "Numpad7", "Numpad8", "Numpad9":
		return true
	default:
		return false
	}
}

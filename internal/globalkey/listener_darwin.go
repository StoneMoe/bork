//go:build darwin && !ios && cgo

package globalkey

/*
#cgo LDFLAGS: -framework CoreGraphics -framework CoreFoundation
#include <stdint.h>

typedef struct bork_key_listener bork_key_listener;

bork_key_listener *bork_key_listener_create(uint16_t key_code, uintptr_t callback_handle);
void bork_key_listener_run(bork_key_listener *listener);
void bork_key_listener_stop(bork_key_listener *listener);
void bork_key_listener_destroy(bork_key_listener *listener);
*/
import "C"

import (
	"errors"
	"runtime"
	"runtime/cgo"
)

type platformListener struct {
	owner *Listener
	state *C.bork_key_listener
	done  chan struct{}
}

type darwinStartResult struct {
	state *C.bork_key_listener
	done  chan struct{}
}

func (p *platformListener) initPlatform(owner *Listener) {
	p.owner = owner
}

func (p *platformListener) startPlatform(code string) error {
	keyCode, ok := darwinKeyCodes[code]
	if !ok {
		return errors.New("push-to-talk key is unsupported on macOS")
	}

	// Event taps must be attached to a run loop. Keep that loop on one native
	// thread and return only after the tap has either opened or failed.
	ready := make(chan darwinStartResult, 1)
	callback := cgo.NewHandle(p.owner)
	go runDarwinListener(keyCode, callback, ready)
	result := <-ready
	if result.state == nil {
		<-result.done
		return errors.New("global keyboard access is unavailable; allow Bork in macOS Input Monitoring settings")
	}
	p.state = result.state
	p.done = result.done
	return nil
}

func runDarwinListener(keyCode uint16, callback cgo.Handle, ready chan<- darwinStartResult) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer callback.Delete()

	done := make(chan struct{})
	state := C.bork_key_listener_create(C.uint16_t(keyCode), C.uintptr_t(callback))
	ready <- darwinStartResult{state: state, done: done}
	if state != nil {
		C.bork_key_listener_run(state)
		C.bork_key_listener_destroy(state)
	}
	close(done)
}

func (p *platformListener) stopPlatform() {
	if p.state == nil {
		return
	}
	state, done := p.state, p.done
	p.state, p.done = nil, nil
	C.bork_key_listener_stop(state)
	<-done
}

//export borkGlobalKeyEvent
func borkGlobalKeyEvent(callbackHandle C.uintptr_t, pressed C.int) {
	listener := cgo.Handle(callbackHandle).Value().(*Listener)
	listener.setPressed(pressed != 0)
}

// Values are hardware-independent macOS virtual key codes. The table covers
// every KeyboardEvent.code accepted by ValidCode.
var darwinKeyCodes = map[string]uint16{
	"KeyA": 0x00, "KeyS": 0x01, "KeyD": 0x02, "KeyF": 0x03, "KeyH": 0x04, "KeyG": 0x05,
	"KeyZ": 0x06, "KeyX": 0x07, "KeyC": 0x08, "KeyV": 0x09, "IntlBackslash": 0x0A, "KeyB": 0x0B,
	"KeyQ": 0x0C, "KeyW": 0x0D, "KeyE": 0x0E, "KeyR": 0x0F, "KeyY": 0x10, "KeyT": 0x11,
	"Digit1": 0x12, "Digit2": 0x13, "Digit3": 0x14, "Digit4": 0x15, "Digit6": 0x16, "Digit5": 0x17,
	"Equal": 0x18, "Digit9": 0x19, "Digit7": 0x1A, "Minus": 0x1B, "Digit8": 0x1C, "Digit0": 0x1D,
	"BracketRight": 0x1E, "KeyO": 0x1F, "KeyU": 0x20, "BracketLeft": 0x21, "KeyI": 0x22, "KeyP": 0x23,
	"Enter": 0x24, "KeyL": 0x25, "KeyJ": 0x26, "Quote": 0x27, "KeyK": 0x28, "Semicolon": 0x29,
	"Backslash": 0x2A, "Comma": 0x2B, "Slash": 0x2C, "KeyN": 0x2D, "KeyM": 0x2E, "Period": 0x2F,
	"Tab": 0x30, "Space": 0x31, "Backquote": 0x32, "Backspace": 0x33,
	"NumpadDecimal": 0x41, "NumpadMultiply": 0x43, "NumpadAdd": 0x45, "NumpadDivide": 0x4B,
	"NumpadEnter": 0x4C, "NumpadSubtract": 0x4E, "Numpad0": 0x52, "Numpad1": 0x53,
	"Numpad2": 0x54, "Numpad3": 0x55, "Numpad4": 0x56, "Numpad5": 0x57, "Numpad6": 0x58,
	"Numpad7": 0x59, "Numpad8": 0x5B, "Numpad9": 0x5C,
	"F5": 0x60, "F6": 0x61, "F7": 0x62, "F3": 0x63, "F8": 0x64, "F9": 0x65,
	"F11": 0x67, "F10": 0x6D, "F12": 0x6F, "Insert": 0x72, "Home": 0x73,
	"PageUp": 0x74, "Delete": 0x75, "F4": 0x76, "End": 0x77, "F2": 0x78,
	"PageDown": 0x79, "F1": 0x7A, "ArrowLeft": 0x7B, "ArrowRight": 0x7C,
	"ArrowDown": 0x7D, "ArrowUp": 0x7E,
}

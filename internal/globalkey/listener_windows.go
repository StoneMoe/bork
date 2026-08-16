//go:build windows

package globalkey

import (
	"fmt"
	"runtime"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	keyboardLowLevelHook = 13
	keyDownMessage       = 0x0100
	keyUpMessage         = 0x0101
	systemKeyDownMessage = 0x0104
	systemKeyUpMessage   = 0x0105
	quitMessage          = 0x0012
	extendedKeyFlag      = 0x01
	extendedKeyMask      = 1 << 16
)

var (
	user32                   = windows.NewLazySystemDLL("user32.dll")
	setWindowsHookProc       = user32.NewProc("SetWindowsHookExW")
	unhookWindowsHookProc    = user32.NewProc("UnhookWindowsHookEx")
	callNextWindowsHookProc  = user32.NewProc("CallNextHookEx")
	getWindowsMessageProc    = user32.NewProc("GetMessageW")
	peekWindowsMessageProc   = user32.NewProc("PeekMessageW")
	postWindowsThreadMessage = user32.NewProc("PostThreadMessageW")
)

type platformListener struct {
	owner    *Listener
	callback uintptr
	key      atomic.Uint32

	threadID uint32
	done     chan struct{}
}

type hookStartResult struct {
	threadID uint32
	err      error
}

type keyboardHookEvent struct {
	virtualKey uint32
	scanCode   uint32
	flags      uint32
	time       uint32
	extraInfo  uintptr
}

type windowsPoint struct {
	x int32
	y int32
}

type windowsMessage struct {
	window  uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	point   windowsPoint
	private uint32
}

func (p *platformListener) initPlatform(owner *Listener) {
	p.owner = owner
	// Windows callback slots live for the whole process, so create one callback
	// and reuse it across every room.
	p.callback = windows.NewCallback(p.handleHook)
}

func (p *platformListener) startPlatform(code string) error {
	key, ok := windowsPhysicalKeys[code]
	if !ok {
		return fmt.Errorf("push-to-talk key %q is unsupported on Windows", code)
	}

	p.key.Store(key)

	ready := make(chan hookStartResult, 1)
	done := make(chan struct{})
	go p.runHookThread(ready, done)
	result := <-ready
	if result.err != nil {
		<-done
		p.key.Store(0)
		return result.err
	}
	p.threadID = result.threadID
	p.done = done
	return nil
}

func (p *platformListener) stopPlatform() {
	p.key.Store(0)
	if p.done == nil {
		return
	}

	_, _, _ = postWindowsThreadMessage.Call(uintptr(p.threadID), quitMessage, 0, 0)
	<-p.done
	p.threadID = 0
	p.done = nil
}

func (p *platformListener) runHookThread(ready chan<- hookStartResult, done chan<- struct{}) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(done)
	defer p.owner.setPressed(false)

	threadID := windows.GetCurrentThreadId()
	var message windowsMessage
	// PostThreadMessage only works after the target thread owns a message queue.
	_, _, _ = peekWindowsMessageProc.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0, 0)

	var module windows.Handle
	if err := windows.GetModuleHandleEx(windows.GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT, nil, &module); err != nil {
		ready <- hookStartResult{err: fmt.Errorf("get application module: %w", err)}
		return
	}
	hook, _, callErr := setWindowsHookProc.Call(keyboardLowLevelHook, p.callback, uintptr(module), 0)
	if hook == 0 {
		ready <- hookStartResult{err: fmt.Errorf("start global keyboard listener: %w", windowsCallError(callErr))}
		return
	}
	defer func() { _, _, _ = unhookWindowsHookProc.Call(hook) }()

	ready <- hookStartResult{threadID: threadID}
	for {
		result, _, _ := getWindowsMessageProc.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0)
		if result == 0 || int32(result) == -1 {
			return
		}
	}
}

func (p *platformListener) handleHook(code int, message uintptr, event *keyboardHookEvent) uintptr {
	if code >= 0 {
		key := event.scanCode
		if event.flags&extendedKeyFlag != 0 {
			key |= extendedKeyMask
		}
		if key == p.key.Load() {
			switch message {
			case keyDownMessage, systemKeyDownMessage:
				p.owner.setPressed(true)
			case keyUpMessage, systemKeyUpMessage:
				p.owner.setPressed(false)
			}
		}
	}
	result, _, _ := callNextWindowsHookProc.Call(0, uintptr(code), message, uintptr(unsafe.Pointer(event)))
	return result
}

func windowsCallError(err error) error {
	if number, ok := err.(syscall.Errno); ok && number != 0 {
		return number
	}
	return syscall.EINVAL
}

var windowsPhysicalKeys = map[string]uint32{
	"Backquote":      0x29,
	"Minus":          0x0c,
	"Equal":          0x0d,
	"Backspace":      0x0e,
	"Tab":            0x0f,
	"BracketLeft":    0x1a,
	"BracketRight":   0x1b,
	"Backslash":      0x2b,
	"Semicolon":      0x27,
	"Quote":          0x28,
	"Enter":          0x1c,
	"Comma":          0x33,
	"Period":         0x34,
	"Slash":          0x35,
	"Space":          0x39,
	"IntlBackslash":  0x56,
	"Insert":         extendedKeyMask | 0x52,
	"Home":           extendedKeyMask | 0x47,
	"PageUp":         extendedKeyMask | 0x49,
	"Delete":         extendedKeyMask | 0x53,
	"End":            extendedKeyMask | 0x4f,
	"PageDown":       extendedKeyMask | 0x51,
	"ArrowLeft":      extendedKeyMask | 0x4b,
	"ArrowRight":     extendedKeyMask | 0x4d,
	"ArrowUp":        extendedKeyMask | 0x48,
	"ArrowDown":      extendedKeyMask | 0x50,
	"NumpadDivide":   extendedKeyMask | 0x35,
	"NumpadMultiply": 0x37,
	"NumpadSubtract": 0x4a,
	"NumpadAdd":      0x4e,
	"NumpadEnter":    extendedKeyMask | 0x1c,
	"NumpadDecimal":  0x53,
	"Numpad0":        0x52,
	"Numpad1":        0x4f,
	"Numpad2":        0x50,
	"Numpad3":        0x51,
	"Numpad4":        0x4b,
	"Numpad5":        0x4c,
	"Numpad6":        0x4d,
	"Numpad7":        0x47,
	"Numpad8":        0x48,
	"Numpad9":        0x49,
	"Digit1":         0x02,
	"Digit2":         0x03,
	"Digit3":         0x04,
	"Digit4":         0x05,
	"Digit5":         0x06,
	"Digit6":         0x07,
	"Digit7":         0x08,
	"Digit8":         0x09,
	"Digit9":         0x0a,
	"Digit0":         0x0b,
	"KeyA":           0x1e,
	"KeyB":           0x30,
	"KeyC":           0x2e,
	"KeyD":           0x20,
	"KeyE":           0x12,
	"KeyF":           0x21,
	"KeyG":           0x22,
	"KeyH":           0x23,
	"KeyI":           0x17,
	"KeyJ":           0x24,
	"KeyK":           0x25,
	"KeyL":           0x26,
	"KeyM":           0x32,
	"KeyN":           0x31,
	"KeyO":           0x18,
	"KeyP":           0x19,
	"KeyQ":           0x10,
	"KeyR":           0x13,
	"KeyS":           0x1f,
	"KeyT":           0x14,
	"KeyU":           0x16,
	"KeyV":           0x2f,
	"KeyW":           0x11,
	"KeyX":           0x2d,
	"KeyY":           0x15,
	"KeyZ":           0x2c,
	"F1":             0x3b,
	"F2":             0x3c,
	"F3":             0x3d,
	"F4":             0x3e,
	"F5":             0x3f,
	"F6":             0x40,
	"F7":             0x41,
	"F8":             0x42,
	"F9":             0x43,
	"F10":            0x44,
	"F11":            0x57,
	"F12":            0x58,
}

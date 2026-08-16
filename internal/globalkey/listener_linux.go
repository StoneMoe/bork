//go:build linux && !android

package globalkey

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	portalDestination      = "org.freedesktop.portal.Desktop"
	portalPath             = dbus.ObjectPath("/org/freedesktop/portal/desktop")
	portalShortcuts        = "org.freedesktop.portal.GlobalShortcuts"
	portalRequest          = "org.freedesktop.portal.Request"
	portalSession          = "org.freedesktop.portal.Session"
	portalShortcutID       = "push-to-talk"
	portalSetupTimeout     = 60 * time.Second
	portalCleanupTimeout   = 2 * time.Second
	portalSignalBufferSize = 16
)

type platformListener struct {
	owner *Listener

	mu  sync.Mutex
	run *portalRun
}

type portalRun struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

type portalShortcut struct {
	ID      string
	Options map[string]dbus.Variant
}

func (p *platformListener) initPlatform(owner *Listener) {
	p.owner = owner
}

func (p *platformListener) startPlatform(code string) error {
	trigger, ok := linuxTrigger(code)
	if !ok {
		return errors.New("push-to-talk key is unsupported on Linux")
	}

	ctx, cancel := context.WithCancel(context.Background())
	run := &portalRun{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	p.mu.Lock()
	p.run = run
	p.mu.Unlock()

	// Portal registration may display a desktop-owned dialog. Do it outside the
	// Wails command so joining or leaving a room never waits for that dialog.
	go p.runPortalInBackground(run, trigger)
	return nil
}

func (p *platformListener) stopPlatform() {
	p.stopCurrent()
}

func (p *platformListener) stopCurrent() {
	p.mu.Lock()
	run := p.run
	p.run = nil
	p.mu.Unlock()
	if run == nil {
		return
	}
	run.cancel()
	<-run.done
}

func (p *platformListener) runPortalInBackground(run *portalRun, trigger string) {
	err := p.runPortal(run.ctx, trigger)
	p.mu.Lock()
	current := p.run == run
	if current {
		// Clear the key before exposing an empty slot to a possible rebind.
		// Otherwise a finished old session could mute a newly started one.
		p.owner.setPressed(false)
		p.run = nil
		if err != nil && !errors.Is(err, context.Canceled) {
			p.owner.setError(fmt.Errorf("start global push-to-talk key: %w", err))
		}
	}
	p.mu.Unlock()
	close(run.done)
}

func (p *platformListener) runPortal(ctx context.Context, trigger string) error {
	handler := newPortalSignalHandler()
	connection, err := dbus.ConnectSessionBus(dbus.WithSignalHandler(handler))
	if err != nil {
		return fmt.Errorf("connect to desktop portal: %w", err)
	}
	defer connection.Close()

	setupCtx, cancelSetup := context.WithTimeout(ctx, portalSetupTimeout)
	defer cancelSetup()
	if err := addPortalSignalMatches(setupCtx, connection); err != nil {
		return err
	}
	desktop := connection.Object(portalDestination, portalPath)
	session, err := createShortcutSession(setupCtx, connection, desktop, handler)
	if err != nil {
		return err
	}
	defer closePortalSession(connection, session)
	if err := bindPushToTalkShortcut(setupCtx, connection, desktop, handler, session, trigger); err != nil {
		return err
	}
	cancelSetup()
	return p.readPortalKeyEvents(ctx, handler, session)
}

func addPortalSignalMatches(ctx context.Context, connection *dbus.Conn) error {
	matches := [][]dbus.MatchOption{
		{dbus.WithMatchInterface(portalRequest), dbus.WithMatchMember("Response")},
		{dbus.WithMatchInterface(portalShortcuts), dbus.WithMatchMember("Activated")},
		{dbus.WithMatchInterface(portalShortcuts), dbus.WithMatchMember("Deactivated")},
		{dbus.WithMatchInterface(portalSession), dbus.WithMatchMember("Closed")},
	}
	for _, match := range matches {
		if err := connection.AddMatchSignalContext(ctx, match...); err != nil {
			return fmt.Errorf("subscribe to desktop portal: %w", err)
		}
	}
	return nil
}

func createShortcutSession(ctx context.Context, connection *dbus.Conn, desktop dbus.BusObject, handler *portalSignalHandler) (dbus.ObjectPath, error) {
	options := map[string]dbus.Variant{
		"handle_token":         dbus.MakeVariant("bork_create"),
		"session_handle_token": dbus.MakeVariant("bork_session"),
	}
	var requestPath dbus.ObjectPath
	if err := desktop.CallWithContext(ctx, portalShortcuts+".CreateSession", 0, options).Store(&requestPath); err != nil {
		return "", fmt.Errorf("create global shortcuts session: %w", err)
	}
	results, err := waitPortalResponse(ctx, connection, handler, requestPath, "create global shortcuts session")
	if err != nil {
		return "", err
	}
	return portalSessionPath(results)
}

func portalSessionPath(results map[string]dbus.Variant) (dbus.ObjectPath, error) {
	value, ok := results["session_handle"]
	if !ok {
		return "", errors.New("global shortcuts portal omitted the session handle")
	}
	var session string
	if err := value.Store(&session); err != nil {
		return "", fmt.Errorf("read global shortcuts session: %w", err)
	}
	path := dbus.ObjectPath(session)
	if !path.IsValid() {
		return "", errors.New("global shortcuts portal returned an invalid session handle")
	}
	return path, nil
}

func bindPushToTalkShortcut(ctx context.Context, connection *dbus.Conn, desktop dbus.BusObject, handler *portalSignalHandler, session dbus.ObjectPath, trigger string) error {
	shortcuts := []portalShortcut{{
		ID: portalShortcutID,
		Options: map[string]dbus.Variant{
			"description":       dbus.MakeVariant("按键说话"),
			"preferred_trigger": dbus.MakeVariant(trigger),
		},
	}}
	options := map[string]dbus.Variant{"handle_token": dbus.MakeVariant("bork_bind")}
	var requestPath dbus.ObjectPath
	if err := desktop.CallWithContext(ctx, portalShortcuts+".BindShortcuts", 0, session, shortcuts, "", options).Store(&requestPath); err != nil {
		return fmt.Errorf("bind push-to-talk key: %w", err)
	}
	results, err := waitPortalResponse(ctx, connection, handler, requestPath, "bind push-to-talk key")
	if err != nil {
		return err
	}
	value, ok := results["shortcuts"]
	if !ok {
		return errors.New("global shortcuts portal did not bind the push-to-talk key")
	}
	var bound []portalShortcut
	if err := value.Store(&bound); err != nil {
		return fmt.Errorf("read bound push-to-talk key: %w", err)
	}
	// Only one shortcut was requested, so the portal's subset must contain
	// exactly that entry for push-to-talk to be usable.
	if len(bound) != 1 || bound[0].ID != portalShortcutID {
		return errors.New("global shortcuts portal did not bind the push-to-talk key")
	}
	return nil
}

func waitPortalResponse(ctx context.Context, connection *dbus.Conn, handler *portalSignalHandler, requestPath dbus.ObjectPath, action string) (map[string]dbus.Variant, error) {
	for {
		select {
		case <-ctx.Done():
			closePortalRequest(connection, requestPath)
			return nil, ctx.Err()
		case <-handler.done:
			return nil, errors.New("desktop portal connection closed")
		case signal := <-handler.signals:
			results, matched, err := decodePortalResponse(signal, requestPath, action)
			if matched {
				return results, err
			}
		}
	}
}

func decodePortalResponse(signal *dbus.Signal, requestPath dbus.ObjectPath, action string) (map[string]dbus.Variant, bool, error) {
	if signal.Path != requestPath || signal.Name != portalRequest+".Response" {
		return nil, false, nil
	}
	var response uint32
	var results map[string]dbus.Variant
	if err := dbus.Store(signal.Body, &response, &results); err != nil {
		return nil, true, fmt.Errorf("%s: read portal response: %w", action, err)
	}
	if response != 0 {
		return nil, true, fmt.Errorf("%s: portal response %d", action, response)
	}
	return results, true, nil
}

func (p *platformListener) readPortalKeyEvents(ctx context.Context, handler *portalSignalHandler, session dbus.ObjectPath) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-handler.done:
			return errors.New("desktop portal connection closed")
		case signal := <-handler.signals:
			pressed, matched, err := portalKeyState(signal, session)
			if err != nil {
				return err
			}
			if matched {
				p.owner.setPressed(pressed)
			}
		}
	}
}

func portalKeyState(signal *dbus.Signal, session dbus.ObjectPath) (bool, bool, error) {
	if signal.Name == portalSession+".Closed" && signal.Path == session {
		return false, false, errors.New("global shortcuts portal closed the session")
	}
	pressed := signal.Name == portalShortcuts+".Activated"
	if !pressed && signal.Name != portalShortcuts+".Deactivated" {
		return false, false, nil
	}
	var eventSession dbus.ObjectPath
	var shortcutID string
	var timestamp uint64
	var options map[string]dbus.Variant
	if err := dbus.Store(signal.Body, &eventSession, &shortcutID, &timestamp, &options); err != nil {
		return false, false, fmt.Errorf("read global shortcut event: %w", err)
	}
	return pressed, eventSession == session && shortcutID == portalShortcutID, nil
}

func closePortalRequest(connection *dbus.Conn, path dbus.ObjectPath) {
	ctx, cancel := context.WithTimeout(context.Background(), portalCleanupTimeout)
	defer cancel()
	_ = connection.Object(portalDestination, path).CallWithContext(ctx, portalRequest+".Close", 0).Err
}

func closePortalSession(connection *dbus.Conn, path dbus.ObjectPath) {
	ctx, cancel := context.WithTimeout(context.Background(), portalCleanupTimeout)
	defer cancel()
	_ = connection.Object(portalDestination, path).CallWithContext(ctx, portalSession+".Close", 0).Err
}

func linuxTrigger(code string) (string, bool) {
	if len(code) == 4 && strings.HasPrefix(code, "Key") {
		return strings.ToLower(code[3:]), true
	}
	if len(code) == 6 && strings.HasPrefix(code, "Digit") {
		return code[5:], true
	}
	if validFunctionKey(code) {
		return code, true
	}
	trigger, ok := linuxNamedTriggers[code]
	return trigger, ok
}

var linuxNamedTriggers = map[string]string{
	"Backquote": "grave", "Minus": "minus", "Equal": "equal", "Backspace": "BackSpace", "Tab": "Tab",
	"BracketLeft": "bracketleft", "BracketRight": "bracketright", "Backslash": "backslash",
	"Semicolon": "semicolon", "Quote": "apostrophe", "Enter": "Return", "Comma": "comma",
	"Period": "period", "Slash": "slash", "Space": "space", "IntlBackslash": "less",
	"Insert": "Insert", "Home": "Home", "PageUp": "Page_Up", "Delete": "Delete", "End": "End",
	"PageDown": "Page_Down", "ArrowLeft": "Left", "ArrowRight": "Right", "ArrowUp": "Up", "ArrowDown": "Down",
	"NumpadDivide": "KP_Divide", "NumpadMultiply": "KP_Multiply", "NumpadSubtract": "KP_Subtract",
	"NumpadAdd": "KP_Add", "NumpadEnter": "KP_Enter", "NumpadDecimal": "KP_Decimal",
	"Numpad0": "KP_0", "Numpad1": "KP_1", "Numpad2": "KP_2", "Numpad3": "KP_3", "Numpad4": "KP_4",
	"Numpad5": "KP_5", "Numpad6": "KP_6", "Numpad7": "KP_7", "Numpad8": "KP_8", "Numpad9": "KP_9",
}

// A bounded handler keeps a broken or noisy portal from growing goroutines or
// memory. Blocking the private D-Bus reader is safe until the room cancels it.
type portalSignalHandler struct {
	signals chan *dbus.Signal
	done    chan struct{}
	once    sync.Once
}

func newPortalSignalHandler() *portalSignalHandler {
	return &portalSignalHandler{signals: make(chan *dbus.Signal, portalSignalBufferSize), done: make(chan struct{})}
}

func (h *portalSignalHandler) DeliverSignal(iface, name string, signal *dbus.Signal) {
	fullName := iface + "." + name
	switch fullName {
	case portalRequest + ".Response", portalShortcuts + ".Activated", portalShortcuts + ".Deactivated", portalSession + ".Closed":
		select {
		case h.signals <- signal:
		case <-h.done:
		}
	}
}

func (h *portalSignalHandler) Terminate() {
	h.once.Do(func() { close(h.done) })
}

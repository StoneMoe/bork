package netfilter

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"bork/internal/gameproxy/intercept"
)

func TestNewBridge_builds_exact_outbound_IPv4_rules_in_path_order(t *testing.T) {
	paths := []string{`c:\games\alpha.exe`, `\\server\share\bravo.exe`}
	backend := &fakeNativeBackend{}
	bridge, err := newBridge(paths, backend)
	if err != nil {
		t.Fatal(err)
	}
	paths[0] = `c:\changed.exe`

	err = bridge.Start(context.Background(), callbackStub{})

	if err != nil {
		t.Fatal(err)
	}
	want := []nativeRule{
		{direction: nativeDirectionOutbound, family: nativeAddressFamilyIPv4, protocol: nativeProtocolTCP, flags: nativeFlagFilter | nativeFlagOffline, executablePath: `c:\games\alpha.exe`},
		{direction: nativeDirectionOutbound, family: nativeAddressFamilyIPv4, protocol: nativeProtocolUDP, flags: nativeFlagFilter, executablePath: `c:\games\alpha.exe`},
		{direction: nativeDirectionOutbound, family: nativeAddressFamilyIPv4, protocol: nativeProtocolTCP, flags: nativeFlagFilter | nativeFlagOffline, executablePath: `\\server\share\bravo.exe`},
		{direction: nativeDirectionOutbound, family: nativeAddressFamilyIPv4, protocol: nativeProtocolUDP, flags: nativeFlagFilter, executablePath: `\\server\share\bravo.exe`},
	}
	if got := backend.rulesSnapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("native rules = %#v, want %#v", got, want)
	}
	if got := backend.eventsSnapshot(); !reflect.DeepEqual(got, []string{"start"}) {
		t.Fatalf("native calls = %q, want [start]", got)
	}
}

func TestNativeRuleConstants_match_verified_SDK_header_values(t *testing.T) {
	if nativeDirectionOutbound != 2 || nativeAddressFamilyIPv4 != 2 || nativeFlagFilter != 2 || nativeFlagOffline != 8 {
		t.Fatalf("native constants = direction %d, family %d, filter %d, offline %d", nativeDirectionOutbound, nativeAddressFamilyIPv4, nativeFlagFilter, nativeFlagOffline)
	}
	if nativeProtocolTCP != 6 || nativeProtocolUDP != 17 {
		t.Fatalf("native protocols = TCP %d, UDP %d", nativeProtocolTCP, nativeProtocolUDP)
	}
}

func TestNewBridge_rejects_invalid_executable_paths(t *testing.T) {
	tests := []struct {
		name  string
		paths []string
		want  error
	}{
		{name: "empty list", want: ErrEmptyExecutablePaths},
		{name: "empty path", paths: []string{""}, want: ErrInvalidExecutablePath},
		{name: "embedded NUL", paths: []string{"c:\\game\x00.exe"}, want: ErrInvalidExecutablePath},
		{name: "relative path", paths: []string{`game.exe`}, want: ErrInvalidExecutablePath},
		{name: "drive relative path", paths: []string{`c:game.exe`}, want: ErrInvalidExecutablePath},
		{name: "POSIX path", paths: []string{`/games/game.exe`}, want: ErrInvalidExecutablePath},
		{name: "incomplete UNC path", paths: []string{`\\server`}, want: ErrInvalidExecutablePath},
		{name: "extended drive namespace", paths: []string{`\\?\c:\games\game.exe`}, want: ErrInvalidExecutablePath},
		{name: "device namespace", paths: []string{`\\.\c:\games\game.exe`}, want: ErrInvalidExecutablePath},
		{name: "duplicate exact path", paths: []string{`c:\games\game.exe`, `c:\games\game.exe`}, want: ErrDuplicateExecutablePath},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newBridge(test.paths, &fakeNativeBackend{})

			if !errors.Is(err, test.want) {
				t.Fatalf("newBridge() error = %v, want %v", err, test.want)
			}
			if len(test.paths) > 0 {
				var pathErr *ExecutablePathError
				if !errors.As(err, &pathErr) {
					t.Fatalf("newBridge() error type = %T, want *ExecutablePathError", err)
				}
			}
		})
	}
}

func TestNewBridge_rejects_nil_backend(t *testing.T) {
	_, err := newBridge([]string{`c:\games\game.exe`}, nil)

	if !errors.Is(err, ErrNilBackend) {
		t.Fatalf("newBridge() error = %v, want ErrNilBackend", err)
	}
}

func TestBridge_Start_installs_callbacks_before_native_start(t *testing.T) {
	backend := &fakeNativeBackend{}
	bridge := newTestBridge(t, backend)
	backend.onStart = func(sink nativeCallbackSink) error {
		if sink != bridge {
			t.Fatalf("native callback sink = %T %p, want bridge %p", sink, sink, bridge)
		}
		bridge.mu.Lock()
		callbacksReady := bridge.callbacks != nil
		bridge.mu.Unlock()
		if !callbacksReady {
			t.Fatal("callbacks were not installed during native Start")
		}
		return nil
	}

	err := bridge.Start(context.Background(), callbackStub{})

	if err != nil {
		t.Fatal(err)
	}
}

func TestBridge_Start_rolls_back_native_failure_and_joins_close_failure(t *testing.T) {
	startErr := errors.New("set exact rules")
	closeErr := errors.New("release native resources")
	backend := &fakeNativeBackend{startErr: startErr, closeErr: closeErr}
	bridge := newTestBridge(t, backend)

	err := bridge.Start(context.Background(), callbackStub{})

	if !errors.Is(err, startErr) || !errors.Is(err, closeErr) {
		t.Fatalf("Start() error = %v, want joined start and close failures", err)
	}
	if got := backend.eventsSnapshot(); !reflect.DeepEqual(got, []string{"start", "close"}) {
		t.Fatalf("native calls = %q, want [start close]", got)
	}
	if got := backend.closeCount(); got != 1 {
		t.Fatalf("native Close calls = %d, want 1", got)
	}
}

func TestBridge_Wait_returns_native_fatal_error_unchanged(t *testing.T) {
	fatalErr := errors.New("native callback loop failed")
	backend := &fakeNativeBackend{waitErr: fatalErr}
	bridge := newTestBridge(t, backend)
	if err := bridge.Start(context.Background(), callbackStub{}); err != nil {
		t.Fatal(err)
	}

	err := bridge.Wait(context.Background())

	if err != fatalErr {
		t.Fatalf("Wait() error = %v, want unchanged native error %v", err, fatalErr)
	}
}

func TestBridge_Close_is_concurrent_idempotent_and_returns_same_error(t *testing.T) {
	closeErr := errors.New("native close failed")
	backend := &fakeNativeBackend{closeErr: closeErr}
	bridge := newTestBridge(t, backend)
	if err := bridge.Start(context.Background(), callbackStub{}); err != nil {
		t.Fatal(err)
	}

	const callers = 32
	results := make(chan error, callers)
	var group sync.WaitGroup
	group.Add(callers)
	for range callers {
		go func() {
			defer group.Done()
			results <- bridge.Close()
		}()
	}
	group.Wait()
	close(results)

	for err := range results {
		if !errors.Is(err, closeErr) {
			t.Fatalf("Close() error = %v, want native close failure", err)
		}
	}
	if got := backend.closeCount(); got != 1 {
		t.Fatalf("native Close calls = %d, want 1", got)
	}
}

func newTestBridge(t *testing.T, backend nativeBackend) *Bridge {
	t.Helper()
	bridge, err := newBridge([]string{`c:\games\game.exe`}, backend)
	if err != nil {
		t.Fatal(err)
	}
	return bridge
}

type callbackStub struct{}

func (callbackStub) TCP(context.Context, intercept.NativeTCPFlow) error     { return nil }
func (callbackStub) UDP(context.Context, intercept.NativeUDPEndpoint) error { return nil }
func (callbackStub) GenerationState() intercept.GenerationState             { return intercept.GenerationState{} }

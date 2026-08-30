package netfilter

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

func TestBridge_lifecycle_rejections_return_sentinel_errors(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Bridge) error
		want error
	}{
		{name: "nil callbacks", run: func(bridge *Bridge) error { return bridge.Start(context.Background(), nil) }, want: ErrNilCallbacks},
		{name: "wait before start", run: func(bridge *Bridge) error { return bridge.Wait(context.Background()) }, want: ErrNotStarted},
		{name: "duplicate start", run: func(bridge *Bridge) error {
			if err := bridge.Start(context.Background(), callbackStub{}); err != nil {
				return err
			}
			return bridge.Start(context.Background(), callbackStub{})
		}, want: ErrAlreadyStarted},
		{name: "start after close", run: func(bridge *Bridge) error {
			if err := bridge.Close(); err != nil {
				return err
			}
			return bridge.Start(context.Background(), callbackStub{})
		}, want: ErrClosed},
		{name: "wait after close", run: func(bridge *Bridge) error {
			if err := bridge.Close(); err != nil {
				return err
			}
			return bridge.Wait(context.Background())
		}, want: ErrClosed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bridge := newTestBridge(t, &fakeNativeBackend{})

			err := test.run(bridge)

			if !errors.Is(err, test.want) {
				t.Fatalf("lifecycle error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestBridge_Start_returns_pre_cancellation_without_native_call(t *testing.T) {
	backend := &fakeNativeBackend{}
	bridge := newTestBridge(t, backend)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := bridge.Start(ctx, callbackStub{})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context.Canceled", err)
	}
	if got := backend.eventsSnapshot(); len(got) != 0 {
		t.Fatalf("native calls = %q, want none", got)
	}
}

func TestBridge_Wait_propagates_context_cancellation(t *testing.T) {
	waitStarted := make(chan struct{})
	backend := &fakeNativeBackend{waitForContext: true, waitStarted: waitStarted}
	bridge := newTestBridge(t, backend)
	if err := bridge.Start(context.Background(), callbackStub{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- bridge.Wait(ctx) }()
	<-waitStarted

	cancel()

	err := <-result

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want context.Canceled", err)
	}
}

func TestBridge_failed_Start_cannot_retry_and_Close_does_not_retry_native_close(t *testing.T) {
	startErr := errors.New("native start failed")
	backend := &fakeNativeBackend{startErr: startErr}
	bridge := newTestBridge(t, backend)
	if err := bridge.Start(context.Background(), callbackStub{}); !errors.Is(err, startErr) {
		t.Fatalf("first Start() error = %v, want native start failure", err)
	}

	startAgainErr := bridge.Start(context.Background(), callbackStub{})
	closeErr := bridge.Close()

	if !errors.Is(startAgainErr, ErrAlreadyStarted) {
		t.Fatalf("second Start() error = %v, want ErrAlreadyStarted", startAgainErr)
	}
	if closeErr != nil {
		t.Fatalf("Close() error = %v, want nil", closeErr)
	}
	if got := backend.closeCount(); got != 1 {
		t.Fatalf("native Close calls = %d, want 1", got)
	}
}

type fakeNativeBackend struct {
	mu             sync.Mutex
	events         []string
	rules          []nativeRule
	onStart        func(nativeCallbackSink) error
	startErr       error
	waitErr        error
	closeErr       error
	waitForContext bool
	waitStarted    chan struct{}
	closes         int
}

func (backend *fakeNativeBackend) Start(ctx context.Context, sink nativeCallbackSink, rules []nativeRule) error {
	backend.mu.Lock()
	backend.events = append(backend.events, "start")
	backend.rules = append([]nativeRule(nil), rules...)
	hook := backend.onStart
	startErr := backend.startErr
	backend.mu.Unlock()
	if hook != nil {
		if err := hook(sink); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return startErr
}

func (backend *fakeNativeBackend) Wait(ctx context.Context) error {
	backend.mu.Lock()
	backend.events = append(backend.events, "wait")
	waitForContext := backend.waitForContext
	waitStarted := backend.waitStarted
	waitErr := backend.waitErr
	backend.mu.Unlock()
	if waitStarted != nil {
		close(waitStarted)
	}
	if waitForContext {
		<-ctx.Done()
		return ctx.Err()
	}
	return waitErr
}

func (backend *fakeNativeBackend) Close() error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.events = append(backend.events, "close")
	backend.closes++
	return backend.closeErr
}

func (backend *fakeNativeBackend) rulesSnapshot() []nativeRule {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return append([]nativeRule(nil), backend.rules...)
}

func (backend *fakeNativeBackend) eventsSnapshot() []string {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return append([]string(nil), backend.events...)
}

func (backend *fakeNativeBackend) closeCount() int {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.closes
}

func TestFakeNativeBackend_records_stable_call_order(t *testing.T) {
	backend := &fakeNativeBackend{}
	bridge := newTestBridge(t, backend)
	if err := bridge.Start(context.Background(), callbackStub{}); err != nil {
		t.Fatal(err)
	}
	if err := bridge.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := bridge.Close(); err != nil {
		t.Fatal(err)
	}

	if got := backend.eventsSnapshot(); !reflect.DeepEqual(got, []string{"start", "wait", "close"}) {
		t.Fatalf("native calls = %q, want [start wait close]", got)
	}
}

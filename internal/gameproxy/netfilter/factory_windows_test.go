//go:build windows && amd64 && cgo && game_proxy

package netfilter

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFactoryPreparesOnlyAfterValidatingDLL(t *testing.T) {
	factory := NewFactory()
	factory.cacheErr = nil
	factory.materializer.cacheRoot = t.TempDir()
	queries := 0
	factory.preparer = &driverPreparer{
		inspect: func() (driverState, error) {
			queries++
			return driverRunning, nil
		},
		wait: time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := factory.EnsureAvailable(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled preparation = %v", err)
	}
	contents := factory.materializer.spec.contents
	factory.materializer.spec.contents = nil
	if err := factory.EnsureAvailable(context.Background()); err == nil {
		t.Fatal("invalid DLL was accepted")
	}
	if queries != 0 {
		t.Fatal("canceled or invalid payload reached driver preparation")
	}
	factory.materializer.spec.contents = contents
	if err := factory.EnsureAvailable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if queries != 1 {
		t.Fatalf("valid preparation queries = %d, want 1", queries)
	}
}

func TestFactoryNewDoesNotPrepareDriver(t *testing.T) {
	factory := NewFactory()
	factory.cacheErr = nil
	factory.materializer.cacheRoot = t.TempDir()
	// A missing preparer makes any accidental preparation fail without UAC.
	factory.preparer = nil
	bridge, err := factory.New(context.Background(), []string{`C:\Games\game.exe`})
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	backend := bridge.(*Bridge).backend.(*sdkNativeBackend)
	if backend.config.driverName != netFilterDriverName {
		t.Fatalf("driver name = %q, want %q", backend.config.driverName, netFilterDriverName)
	}
}

//go:build !windows || !amd64 || !cgo || !netfilter_sdk

package netfilter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFactory_unsupported_build_returns_package_error_without_touching_cache(t *testing.T) {
	// Given
	cacheRoot := filepath.Join(t.TempDir(), "untouched")
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	factory := NewFactory()

	// When
	availableErr := factory.EnsureAvailable(context.Background())
	bridge, newErr := factory.New(context.Background(), []string{`C:\games\game.exe`})

	// Then
	if factory.Supported() {
		t.Fatal("unsupported factory reports Supported")
	}
	if !errors.Is(availableErr, ErrUnsupported) {
		t.Fatalf("EnsureAvailable error = %v, want ErrUnsupported", availableErr)
	}
	if bridge != nil || !errors.Is(newErr, ErrUnsupported) {
		t.Fatalf("New result = (%v, %v), want (nil, ErrUnsupported)", bridge, newErr)
	}
	if _, err := os.Lstat(cacheRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache root stat error = %v, want os.ErrNotExist", err)
	}
}

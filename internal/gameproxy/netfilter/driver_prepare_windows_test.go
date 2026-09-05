//go:build windows && amd64 && game_proxy

package netfilter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestDriverPreparerDecisions(t *testing.T) {
	for _, test := range []struct {
		name   string
		state  driverState
		err    error
		launch bool
	}{
		{"running", driverRunning, nil, false},
		{"missing", driverMissing, nil, true},
		{"stopped", driverStopped, nil, true},
		{"unknown", driverState(255), errDriverConflict, false},
		{"query failed", driverMissing, windows.ERROR_ACCESS_DENIED, false},
		{"conflict", driverMissing, errDriverConflict, false},
		{"disabled", driverStopped, errDriverDisabled, false},
		{"removing", driverMissing, errDriverRemovalPending, false},
		{"busy", driverMissing, errDriverInstallInProgress, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := test.state
			launches := 0
			preparer := newDriverPreparer("", nil)
			preparer.inspect = func() (driverState, error) {
				if test.state == driverState(255) {
					return state, nil
				}
				return state, test.err
			}
			preparer.launch = func(context.Context) error {
				launches++
				state = driverRunning
				return nil
			}
			if err := preparer.ensure(context.Background()); !errors.Is(err, test.err) {
				t.Fatalf("ensure = %v, want %v", err, test.err)
			}
			if (launches == 1) != test.launch || launches > 1 {
				t.Fatalf("launches = %d, want launch %v", launches, test.launch)
			}
		})
	}
}

func TestDriverPreparerPending(t *testing.T) {
	for _, test := range []struct {
		name string
		next driverState
	}{
		{"running", driverRunning},
		{"stopped", driverStopped},
		{"timeout", driverPending},
	} {
		t.Run(test.name, func(t *testing.T) {
			preparer := newDriverPreparer("", nil)
			preparer.wait = 150 * time.Millisecond
			queries, launches := 0, 0
			preparer.inspect = func() (driverState, error) {
				queries++
				if queries == 1 {
					return driverPending, nil
				}
				if launches != 0 {
					return driverRunning, nil
				}
				return test.next, nil
			}
			preparer.launch = func(context.Context) error { launches++; return nil }
			err := preparer.ensure(context.Background())
			if test.next == driverPending {
				if !errors.Is(err, context.DeadlineExceeded) || launches != 0 {
					t.Fatalf("pending timed out with %v and %d launches", err, launches)
				}
			} else if err != nil || (launches == 1) != (test.next == driverStopped) {
				t.Fatalf("ensure = %v, launches = %d", err, launches)
			}
		})
	}
}

func TestDriverPreparerCancellation(t *testing.T) {
	preparer := newDriverPreparer("", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	preparer.inspect = func() (driverState, error) { t.Fatal("queried after cancellation"); return driverMissing, nil }
	preparer.launch = func(context.Context) error { panic("unexpected elevation") }
	if err := preparer.ensure(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	preparer.inspect = func() (driverState, error) { cancel(); return driverPending, nil }
	if err := preparer.ensure(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestDriverPreparerCancelledLaunchRetainsSingleOwner(t *testing.T) {
	preparer := newDriverPreparer("", nil)
	var launches atomic.Int32
	var state atomic.Uint32
	state.Store(uint32(driverStopped))
	preparer.inspect = func() (driverState, error) { return driverState(state.Load()), nil }
	entered, release := make(chan struct{}), make(chan struct{})
	preparer.launch = func(context.Context) error {
		launches.Add(1)
		close(entered)
		<-release // Models ShellExecuteEx waiting for a late UAC response.
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- preparer.ensure(ctx) }()
	<-entered
	preparer.mu.Lock()
	job := preparer.pending
	preparer.mu.Unlock()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation waited for UAC")
	}
	for i := 0; i < 5; i++ {
		if err := preparer.ensure(context.Background()); !errors.Is(err, errDriverInstallInProgress) {
			t.Fatalf("retry started another UAC: %v", err)
		}
	}
	close(release)
	<-job.done
	if launches.Load() != 1 {
		t.Fatalf("launches = %d", launches.Load())
	}
	// A late success does not memoize readiness or resurrect the canceled call.
	preparer.launch = func(context.Context) error {
		launches.Add(1)
		state.Store(uint32(driverRunning))
		return nil
	}
	if err := preparer.ensure(context.Background()); err != nil || launches.Load() != 2 {
		t.Fatalf("fresh retry = %v, launches = %d", err, launches.Load())
	}
}

func TestDriverPreparerDoesNotTrustHelperSuccess(t *testing.T) {
	preparer := newDriverPreparer("", nil)
	preparer.inspect = func() (driverState, error) { return driverStopped, nil }
	preparer.launch = func(context.Context) error { return nil }
	if err := preparer.ensure(context.Background()); err == nil {
		t.Fatal("accepted success without running driver")
	}
	preparer.launch = func(context.Context) error { return windows.ERROR_CANCELLED }
	if err := preparer.ensure(context.Background()); !errors.Is(err, windows.ERROR_CANCELLED) {
		t.Fatalf("UAC denial = %v", err)
	}
}

func helperTestMaterializer(root string) artifactMaterializer {
	contents := []byte("trusted test helper; not an executable")
	return artifactMaterializer{
		cacheRoot: root,
		spec:      artifactSpec{version: "test", filename: "bork-driver-helper.exe", contents: contents, digest: driverDigest(contents)},
		publish:   os.Link,
	}
}

func TestDriverHelperPinsUserCache(t *testing.T) {
	materializer := helperTestMaterializer(t.TempDir())
	helper, err := pinDriverHelper(context.Background(), materializer)
	if err != nil {
		t.Fatal(err)
	}
	defer helper.close()
	writer, err := os.OpenFile(helper.path, os.O_WRONLY, 0)
	if err == nil {
		writer.Close()
		t.Fatal("pinned helper is writable")
	}
	if err := os.Remove(helper.path); err == nil {
		t.Fatal("pinned helper is deletable")
	}
	parent := filepath.Dir(helper.path)
	if err := os.Rename(parent, parent+"-moved"); err == nil {
		t.Fatal("pinned helper ancestor is replaceable")
	}
	if err := verifyHelperObject(helper.files[len(helper.files)-1], helper.path+"-different", false); !errors.Is(err, errDriverConflict) {
		t.Fatalf("wrong final path = %v", err)
	}
	helper.close()
	if err := os.Remove(helper.path); err != nil {
		t.Fatalf("release did not release leaf: %v", err)
	}
	if err := os.Rename(parent, parent+"-moved"); err != nil {
		t.Fatalf("release did not release ancestor: %v", err)
	}
}

func TestDriverHelperRejectsUnsafeCache(t *testing.T) {
	for _, scenario := range []string{"corrupt", "hardlink", "writer", "directory", "reparse leaf", "reparse ancestor"} {
		t.Run(scenario, func(t *testing.T) {
			materializer := helperTestMaterializer(t.TempDir())
			path := materializer.targetPath()
			parent := filepath.Dir(path)
			if err := os.MkdirAll(parent, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, materializer.spec.contents, 0600); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "corrupt":
				if err := os.WriteFile(path, []byte("foreign cache bytes"), 0600); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, filepath.Join(parent, "other.exe")); err != nil {
					t.Fatal(err)
				}
			case "writer":
				file, err := os.OpenFile(path, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
			case "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "reparse leaf", "reparse ancestor":
				link := path
				if scenario == "reparse ancestor" {
					link = parent
				}
				if err := os.Rename(link, link+"-real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(link+"-real", link); err != nil {
					if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
						t.Skip("symlinks require Windows Developer Mode")
					}
					t.Fatal(err)
				}
			}
			helper, err := pinDriverHelper(context.Background(), materializer)
			if helper != nil {
				helper.close()
			}
			if err == nil {
				t.Fatal("unsafe cache was accepted")
			}
		})
	}
	for _, root := range []string{"", ".", `C:relative`, `\\server\share`, `C:\unsafe:stream`, `C:\trailing.`, `C:\nul` + "\x00"} {
		helper, err := pinDriverHelper(context.Background(), helperTestMaterializer(root))
		if helper != nil {
			helper.close()
		}
		if err == nil {
			t.Fatalf("unsafe cache root accepted: %q", root)
		}
	}
}

func TestDriverHelperRechecksPublishedBytesWhilePinned(t *testing.T) {
	materializer := helperTestMaterializer(t.TempDir())
	materializer.publish = func(temporary, target string) error {
		if err := os.Link(temporary, target); err != nil {
			return err
		}
		// Simulate a cache writer after materializer verification, before pinning.
		changed := append([]byte(nil), materializer.spec.contents...)
		changed[0] ^= 1
		return os.WriteFile(target, changed, 0600)
	}
	helper, err := pinDriverHelper(context.Background(), materializer)
	if helper != nil {
		helper.close()
	}
	if !errors.Is(err, errDriverConflict) {
		t.Fatalf("changed executable was not rejected by pinned digest: %v", err)
	}
}

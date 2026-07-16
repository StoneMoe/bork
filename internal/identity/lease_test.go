package identity

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAcquireAllowsOnlyOneActiveInstance(t *testing.T) {
	identity, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate() error = %v", err)
	}
	lease, err := Acquire(identity)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if _, err := Acquire(identity); !errors.Is(err, ErrAlreadyActive) {
		t.Fatalf("second Acquire() error = %v, want ErrAlreadyActive", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	reacquired, err := Acquire(identity)
	if err != nil {
		t.Fatalf("Acquire() after Close error = %v", err)
	}
	if err := reacquired.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestAcquireAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	localIdentity, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate() error = %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(dir, identityFilename))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	helperDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(helperDir, identityFilename), contents, 0o600); err != nil {
		t.Fatalf("WriteFile() copied identity error = %v", err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestIdentityLeaseHelper$")
	command.Env = append(os.Environ(), "BORK_LEASE_HELPER=1", "BORK_LEASE_DIR="+helperDir)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe() error = %v", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe() error = %v", err)
	}
	if err := command.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	helperDone := false
	t.Cleanup(func() {
		if !helperDone {
			_ = stdin.Close()
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "READY\n" {
		t.Fatalf("helper readiness = %q, %v", line, err)
	}
	if _, err := Acquire(localIdentity); !errors.Is(err, ErrAlreadyActive) {
		t.Fatalf("Acquire() while helper holds lease error = %v, want ErrAlreadyActive", err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatalf("kill helper process: %v", err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("killed helper process exited successfully")
	}
	helperDone = true
	_ = stdin.Close()
	lease, err := Acquire(localIdentity)
	if err != nil {
		t.Fatalf("Acquire() after helper exit error = %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestAcquireDoesNotCreateCacheFiles(t *testing.T) {
	root := t.TempDir()
	cacheRoot := filepath.Join(root, "empty-cache")
	switch runtime.GOOS {
	case "windows":
		t.Setenv("LocalAppData", cacheRoot)
	case "darwin":
		home := filepath.Join(root, "empty-home")
		t.Setenv("HOME", home)
		cacheRoot = filepath.Join(home, "Library", "Caches")
	default:
		t.Setenv("XDG_CACHE_HOME", cacheRoot)
	}
	localIdentity, err := LoadOrCreate(filepath.Join(root, "identity"))
	if err != nil {
		t.Fatalf("LoadOrCreate() error = %v", err)
	}
	lease, err := Acquire(localIdentity)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := os.Stat(cacheRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("identity lease created cache path %q: %v", cacheRoot, err)
	}
}

func TestIdentityLeaseHelper(t *testing.T) {
	if os.Getenv("BORK_LEASE_HELPER") != "1" {
		return
	}
	localIdentity, err := LoadOrCreate(os.Getenv("BORK_LEASE_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := Acquire(localIdentity)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	fmt.Println("READY")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

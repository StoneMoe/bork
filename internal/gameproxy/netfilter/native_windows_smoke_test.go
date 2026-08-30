//go:build windows && amd64 && cgo && netfilter_sdk

package netfilter

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bork/internal/gameproxy/intercept"
)

const (
	smokeEnabledEnv = "BORK_NETFILTER_SMOKE"
	smokeDLLEnv     = "BORK_NETFILTER_SMOKE_DLL"
	smokeDriverEnv  = "BORK_NETFILTER_SMOKE_DRIVER"
	smokeTargetEnv  = "BORK_NETFILTER_SMOKE_TARGET"
)

// TestNetFilterSDKSmoke accepts a preinstalled driver and an absolute nfapi.dll
// path. It verifies exact canonical and lowercase rules, sibling exclusion, two
// init/free rounds, balanced callback admission, and no sink callback after free.
func TestNetFilterSDKSmoke(t *testing.T) {
	if os.Getenv(smokeEnabledEnv) == "" {
		t.Skip("set BORK_NETFILTER_SMOKE=1 to run the preinstalled-driver smoke test")
	}
	dllPath := os.Getenv(smokeDLLEnv)
	if dllPath == "" {
		t.Skip("BORK_NETFILTER_SMOKE_DLL must name an absolute nfapi.dll")
	}
	driverName := os.Getenv(smokeDriverEnv)
	if driverName == "" {
		t.Skip("BORK_NETFILTER_SMOKE_DRIVER must name the preinstalled driver")
	}
	selected, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	sibling := copySmokeExecutable(t, selected)
	for round, rulePath := range []string{selected, strings.ToLower(selected)} {
		t.Run(fmt.Sprintf("round_%d", round+1), func(t *testing.T) {
			runNetFilterSmokeRound(t, dllPath, driverName, selected, sibling, rulePath)
		})
	}
}

func TestNetFilterSmokeHelper(t *testing.T) {
	target := os.Getenv(smokeTargetEnv)
	if target == "" {
		t.Skip("smoke helper runs only as a child process")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp4", target)
	if err != nil {
		return
	}
	defer connection.Close()
	if _, err := connection.Write([]byte("netfilter smoke")); err != nil {
		return
	}
}

func runNetFilterSmokeRound(t *testing.T, dllPath, driverName, selected, sibling, rulePath string) {
	t.Helper()
	backend, err := newNativeBackend(dllPath, driverName)
	if err != nil {
		t.Fatal(err)
	}
	sink := newSmokeNativeSink(backend)
	rules, err := exactRules([]string{rulePath})
	if err != nil {
		t.Fatal(err)
	}
	before := nativeCallbackStats()
	if err := backend.Start(context.Background(), sink, rules); err != nil {
		t.Fatalf("driver must already be installed; Start: %v", err)
	}
	runSelectedSmokeChild(t, selected, sink)
	selectedCount := sink.connectedCount()
	runUnfilteredSmokeChild(t, sibling)
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	after := nativeCallbackStats()
	if after.entered-before.entered != after.exited-before.exited {
		t.Fatalf("callback entries = %d, exits = %d",
			after.entered-before.entered, after.exited-before.exited)
	}
	if got := sink.connectedCount(); got != selectedCount {
		t.Fatalf("sibling callback count changed from %d to %d", selectedCount, got)
	}
	runUnfilteredSmokeChild(t, selected)
	if got := sink.connectedCount(); got != selectedCount {
		t.Fatalf("callback count after free changed from %d to %d", selectedCount, got)
	}
}

func runSelectedSmokeChild(t *testing.T, executable string, sink *smokeNativeSink) {
	t.Helper()
	listener := newSmokeListener(t)
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := smokeCommand(ctx, executable, listener.Addr().String())
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	select {
	case event := <-sink.connected:
		if !strings.EqualFold(event.ExecutablePath, executable) {
			t.Fatalf("callback path = %q, want %q", event.ExecutablePath, executable)
		}
		if err := sink.failure(); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatalf("selected executable produced no callback: %v", ctx.Err())
	}
	select {
	case <-wait:
	case <-ctx.Done():
		t.Fatalf("selected child did not exit: %v", ctx.Err())
	}
}

func runUnfilteredSmokeChild(t *testing.T, executable string) {
	t.Helper()
	listener := newSmokeListener(t)
	defer listener.Close()
	accepted := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			err = connection.Close()
		}
		accepted <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := smokeCommand(ctx, executable, listener.Addr().String()).Run(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-accepted:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatalf("unfiltered child did not connect: %v", ctx.Err())
	}
}

func smokeCommand(ctx context.Context, executable, target string) *exec.Cmd {
	command := exec.CommandContext(ctx, executable, "-test.run=^TestNetFilterSmokeHelper$")
	command.Env = append(os.Environ(), smokeTargetEnv+"="+target)
	return command
}

func newSmokeListener(t *testing.T) *net.TCPListener {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4127, Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	return listener
}

func copySmokeExecutable(t *testing.T, source string) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "unselected-sibling.exe")
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.Create(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(output, input); err != nil {
		closeErr := output.Close()
		t.Fatalf("copy executable: %v; close target: %v", err, closeErr)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	return target
}

type smokeNativeSink struct {
	backend   nativeBackend
	connected chan nativeTCPConnectedEvent
	mu        sync.Mutex
	count     int
	err       error
}

func newSmokeNativeSink(backend nativeBackend) *smokeNativeSink {
	return &smokeNativeSink{backend: backend, connected: make(chan nativeTCPConnectedEvent, 4)}
}

func (*smokeNativeSink) nativeCallbackSink() {}
func (sink *smokeNativeSink) tcpConnected(event nativeTCPConnectedEvent) {
	closeErr := sink.backend.CloseTCP(event.ID)
	sink.mu.Lock()
	sink.count++
	if closeErr != nil && sink.err == nil {
		sink.err = closeErr
	}
	sink.mu.Unlock()
	sink.connected <- event
}
func (*smokeNativeSink) tcpSend(intercept.NativeID, []byte) {}
func (*smokeNativeSink) tcpClosed(intercept.NativeID)       {}
func (*smokeNativeSink) udpCreated(nativeUDPCreatedEvent)   {}
func (*smokeNativeSink) udpSend(nativeUDPSendEvent)         {}
func (*smokeNativeSink) udpClosed(intercept.NativeID)       {}
func (sink *smokeNativeSink) connectedCount() int {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.count
}

func (sink *smokeNativeSink) failure() error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.err
}

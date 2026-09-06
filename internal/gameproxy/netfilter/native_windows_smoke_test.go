//go:build windows && amd64 && cgo && game_proxy

package netfilter

import (
	"context"
	"errors"
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
	smokeNetworkEnv = "BORK_NETFILTER_SMOKE_NETWORK"
)

// TestNetFilterSDKSmoke accepts a running preinstalled driver and an absolute nfapi.dll
// path. It verifies exact canonical and lowercase TCP and UDP rules, sibling
// exclusion, two init/free rounds, balanced callback admission, and no sink
// callback after free.
func TestNetFilterSDKSmoke(t *testing.T) {
	if os.Getenv(smokeEnabledEnv) != "1" {
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
	network := os.Getenv(smokeNetworkEnv)
	if network == "" {
		network = "tcp4"
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, network, target)
	if err != nil {
		t.Fatalf("smoke helper dial: %v", err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte("netfilter smoke")); err != nil {
		t.Fatalf("smoke helper write: %v", err)
	}
}

func runNetFilterSmokeRound(t *testing.T, dllPath, driverName, selected, sibling, rulePath string) {
	t.Helper()
	backend, err := newNativeBackend(dllPath, driverName)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := backend.Close(); closeErr != nil {
			t.Errorf("deferred backend close: %v", closeErr)
		}
	}()
	sink := newSmokeNativeSink(t, backend)
	defer sink.close()
	rules, err := exactRules([]string{rulePath})
	if err != nil {
		t.Fatal(err)
	}
	before := nativeCallbackStats()
	if err := backend.Start(context.Background(), sink, rules); err != nil {
		t.Fatalf("driver must already be installed and running; Start: %v", err)
	}
	runSelectedSmokeChild(t, selected, sink)
	selectedCount := sink.connectedCount()
	runSelectedUDPSmokeChild(t, selected, sink)
	selectedUDPCount := sink.udpCreatedCount()
	runUnfilteredSmokeChild(t, sibling)
	runUnfilteredUDPSmokeChild(t, sibling)
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
	if got := sink.udpCreatedCount(); got != selectedUDPCount {
		t.Fatalf("sibling UDP callback count changed from %d to %d", selectedUDPCount, got)
	}
	runUnfilteredSmokeChild(t, selected)
	runUnfilteredUDPSmokeChild(t, selected)
	if got := sink.connectedCount(); got != selectedCount {
		t.Fatalf("callback count after free changed from %d to %d", selectedCount, got)
	}
	if got := sink.udpCreatedCount(); got != selectedUDPCount {
		t.Fatalf("UDP callback count after free changed from %d to %d", selectedUDPCount, got)
	}
}

func runSelectedUDPSmokeChild(t *testing.T, executable string, sink *smokeNativeSink) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := smokeUDPCommand(ctx, executable)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	select {
	case event := <-sink.udpCreatedEvents:
		if event.ExecutablePath != "" && !strings.EqualFold(event.ExecutablePath, executable) {
			t.Fatalf("UDP callback path = %q, want empty or %q", event.ExecutablePath, executable)
		}
		if err := sink.failure(); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatalf("selected executable produced no UDP callback: %v", ctx.Err())
	}
	select {
	case <-wait:
	case <-ctx.Done():
		t.Fatalf("selected UDP child did not exit: %v", ctx.Err())
	}
}

func runSelectedSmokeChild(t *testing.T, executable string, sink *smokeNativeSink) {
	t.Helper()
	listener := newSmokeTargetListener(t)
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
		if event.ExecutablePath != "" && !strings.EqualFold(event.ExecutablePath, executable) {
			t.Fatalf("callback path = %q, want empty or %q", event.ExecutablePath, executable)
		}
		if err := sink.failure(); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatalf("selected executable produced no callback: %v", ctx.Err())
	}
	select {
	case accepted := <-sink.accepted:
		if accepted.err != nil {
			t.Fatal(accepted.err)
		}
		if string(accepted.payload) != "netfilter smoke" {
			t.Fatalf("redirected payload = %q", accepted.payload)
		}
	case <-ctx.Done():
		t.Fatalf("redirect listener accepted no connection: %v", ctx.Err())
	}
	select {
	case <-wait:
	case <-ctx.Done():
		t.Fatalf("selected child did not exit: %v", ctx.Err())
	}
}

func runUnfilteredSmokeChild(t *testing.T, executable string) {
	t.Helper()
	listener := newSmokeTargetListener(t)
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

func runUnfilteredUDPSmokeChild(t *testing.T, executable string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := smokeUDPCommand(ctx, executable).Run(); err != nil {
		t.Fatal(err)
	}
}

func smokeCommand(ctx context.Context, executable, target string) *exec.Cmd {
	command := exec.CommandContext(ctx, executable, "-test.run=^TestNetFilterSmokeHelper$")
	command.Env = append(os.Environ(), smokeTargetEnv+"="+target)
	return command
}

func smokeUDPCommand(ctx context.Context, executable string) *exec.Cmd {
	command := smokeCommand(ctx, executable, "127.0.0.1:9")
	command.Env = append(command.Env, smokeNetworkEnv+"=udp4")
	return command
}

func newSmokeListener(t *testing.T) *net.TCPListener {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	return listener
}

func newSmokeTargetListener(t *testing.T) *net.TCPListener {
	t.Helper()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range addresses {
		network, ok := address.(*net.IPNet)
		if !ok || network.IP.To4() == nil || !network.IP.IsGlobalUnicast() || network.IP.IsLoopback() {
			continue
		}
		// Use a local interface, not an external server or a bypassed 127/8 target.
		listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: network.IP})
		if err != nil {
			t.Fatal(err)
		}
		return listener
	}
	t.Skip("native TCP smoke requires a non-loopback IPv4 interface")
	return nil
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
	backend          nativeBackend
	listener         *net.TCPListener
	redirectPort     uint16
	connected        chan nativeTCPConnectedEvent
	accepted         chan smokeAcceptedConnection
	udpCreatedEvents chan nativeUDPCreatedEvent
	mu               sync.Mutex
	count            int
	udpCount         int
	err              error
}

type smokeAcceptedConnection struct {
	payload []byte
	err     error
}

func newSmokeNativeSink(t *testing.T, backend nativeBackend) *smokeNativeSink {
	t.Helper()
	listener := newSmokeListener(t)
	sink := &smokeNativeSink{
		backend: backend, connected: make(chan nativeTCPConnectedEvent, 4),
		listener: listener, redirectPort: uint16(listener.Addr().(*net.TCPAddr).Port),
		accepted:         make(chan smokeAcceptedConnection, 4),
		udpCreatedEvents: make(chan nativeUDPCreatedEvent, 4),
	}
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
			payload, readErr := io.ReadAll(connection)
			closeErr := connection.Close()
			sink.accepted <- smokeAcceptedConnection{payload: payload, err: errors.Join(readErr, closeErr)}
		}
	}()
	return sink
}

func (*smokeNativeSink) nativeCallbackSink() {}
func (sink *smokeNativeSink) tcpConnectRequest(event nativeTCPConnectRequestEvent) uint16 {
	sink.mu.Lock()
	sink.count++
	sink.mu.Unlock()
	sink.connected <- event
	return sink.redirectPort
}
func (*smokeNativeSink) tcpConnected(nativeTCPConnectedEvent) {}
func (*smokeNativeSink) tcpSend(intercept.NativeID, []byte)   {}
func (*smokeNativeSink) tcpClosed(intercept.NativeID)         {}
func (sink *smokeNativeSink) udpCreated(event nativeUDPCreatedEvent) {
	sink.mu.Lock()
	sink.udpCount++
	sink.mu.Unlock()
	sink.udpCreatedEvents <- event
}
func (*smokeNativeSink) udpSend(nativeUDPSendEvent)         {}
func (*smokeNativeSink) udpClosed(intercept.NativeID)       {}
func (*smokeNativeSink) endpointError(*NativeCallbackError) {}
func (sink *smokeNativeSink) connectedCount() int {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.count
}

func (sink *smokeNativeSink) udpCreatedCount() int {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.udpCount
}

func (sink *smokeNativeSink) failure() error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.err
}

func (sink *smokeNativeSink) close() {
	_ = sink.listener.Close()
}

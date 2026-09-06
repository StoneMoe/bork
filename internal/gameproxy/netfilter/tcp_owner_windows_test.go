//go:build windows && game_proxy

package netfilter

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"bork/internal/gameproxy/intercept"
)

const tcpOwnerHelperEnv = "BORK_TCP_OWNER_TEST_TARGET"

// This uses ordinary Winsock sockets and the read-only IP Helper table, not NetFilter.
func TestTCPSocketOwner_finds_client_process_using_accepted_tuple(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTCPSocketOwnerHelper$")
	command.Env = append(os.Environ(), tcpOwnerHelperEnv+"="+listener.Addr().String())
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		if command.ProcessState == nil {
			_ = command.Wait()
		}
	})
	connection, err := listener.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var ready [1]byte
	if _, err := io.ReadFull(connection, ready[:]); err != nil {
		t.Fatal(err)
	}
	local := connection.RemoteAddr().(*net.TCPAddr).AddrPort()
	remote := connection.LocalAddr().(*net.TCPAddr).AddrPort()
	pid, err := tcpSocketOwner(local, remote)
	if err != nil || pid != intercept.ProcessID(command.Process.Pid) {
		t.Fatalf("client owner = %d, %v, want child PID %d", pid, err, command.Process.Pid)
	}
	serverPID, err := tcpSocketOwner(remote, local)
	if err != nil || serverPID != intercept.ProcessID(os.Getpid()) {
		t.Fatalf("server owner = %d, %v, want current PID %d", serverPID, err, os.Getpid())
	}
	if _, err := connection.Write(ready[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Read(ready[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("child did not close cleanly: %v", err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("ownership helper failed: %v", err)
	}
}

func TestTCPSocketOwnerHelper(t *testing.T) {
	target := os.Getenv(tcpOwnerHelperEnv)
	if target == "" {
		t.Skip("ownership helper runs only as a child process")
	}
	connection, err := net.DialTimeout("tcp4", target, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	var reply [1]byte
	if _, err := io.ReadFull(connection, reply[:]); err != nil {
		t.Fatal(err)
	}
}

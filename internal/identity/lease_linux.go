//go:build linux

package identity

import (
	"errors"

	"golang.org/x/sys/unix"
)

func acquirePlatformLease(peerID string) (func() error, error) {
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	address := &unix.SockaddrUnix{Name: "@bork.identity." + peerID}
	if err := unix.Bind(fd, address); err != nil {
		_ = unix.Close(fd)
		if errors.Is(err, unix.EADDRINUSE) {
			return nil, ErrAlreadyActive
		}
		return nil, err
	}
	return func() error { return unix.Close(fd) }, nil
}

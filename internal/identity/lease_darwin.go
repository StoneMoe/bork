//go:build darwin

package identity

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net"
	"syscall"
)

func acquirePlatformLease(peerID string) (func() error, error) {
	digest := sha256.Sum256([]byte("bork/identity-lease/v1\x00" + peerID))
	address := &net.TCPAddr{
		IP:   net.IPv4(127, 1+digest[0]%254, digest[1], 1+digest[2]%254),
		Port: 49152 + int(binary.BigEndian.Uint16(digest[3:5])%16384),
	}
	listener, err := net.ListenTCP("tcp4", address)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, ErrAlreadyActive
		}
		return nil, err
	}
	return listener.Close, nil
}

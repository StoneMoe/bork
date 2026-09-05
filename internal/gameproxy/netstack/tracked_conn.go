package netstack

import (
	"io"
	"net"
	"sync"

	"github.com/sagernet/gvisor/pkg/tcpip/adapters/gonet"
)

type trackedConn struct {
	*gonet.TCPConn
	onClose func(io.Closer)
	once    sync.Once
	err     error
}

func newTrackedConn(connection *gonet.TCPConn, onClose func(io.Closer)) *trackedConn {
	return &trackedConn{TCPConn: connection, onClose: onClose}
}

func (connection *trackedConn) Close() error {
	connection.once.Do(func() {
		connection.err = connection.TCPConn.Close()
		connection.onClose(connection)
	})
	return connection.err
}

type trackedPacketConn struct {
	net.PacketConn
	onClose func(io.Closer)
	once    sync.Once
	err     error
}

func newTrackedPacketConn(connection net.PacketConn, onClose func(io.Closer)) *trackedPacketConn {
	return &trackedPacketConn{PacketConn: connection, onClose: onClose}
}

func (connection *trackedPacketConn) Close() error {
	connection.once.Do(func() {
		connection.err = connection.PacketConn.Close()
		connection.onClose(connection)
	})
	return connection.err
}

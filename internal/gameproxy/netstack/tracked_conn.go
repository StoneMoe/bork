package netstack

import (
	"io"
	"net"
	"sync"
)

type trackedConn struct {
	net.Conn
	onClose func(io.Closer)
	once    sync.Once
	err     error
}

func newTrackedConn(connection net.Conn, onClose func(io.Closer)) *trackedConn {
	return &trackedConn{Conn: connection, onClose: onClose}
}

func (connection *trackedConn) Close() error {
	connection.once.Do(func() {
		connection.err = connection.Conn.Close()
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

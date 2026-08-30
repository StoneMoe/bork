package intercept

import (
	"context"
	"io"
	"net"
	"net/netip"
	"time"
)

type Generation uint64
type NativeID uint64
type ProcessID uint32

type Metadata struct {
	Generation     Generation
	NativeID       NativeID
	ProcessID      ProcessID
	ExecutablePath string
	OriginalLocal  netip.AddrPort
	OriginalRemote netip.AddrPort
}

type Datagram struct {
	Metadata Metadata
	Payload  []byte
}

type ExecutableMatcher interface {
	Match(string) (bool, error)
}

type Dialer interface {
	DialTCP(context.Context, netip.AddrPort) (net.Conn, error)
	OpenUDP() (net.PacketConn, error)
}

type NativeTCPFlow interface {
	io.ReadWriteCloser
	Metadata() Metadata
	Reset(error) error
}

type NativeUDPEndpoint interface {
	Metadata() Metadata
	ReadDatagram(context.Context) (Datagram, error)
	WriteDatagram(context.Context, Datagram) error
	Reset(error) error
	Close() error
}

type Callbacks interface {
	TCP(context.Context, NativeTCPFlow) error
	UDP(context.Context, NativeUDPEndpoint) error
	GenerationState() GenerationState
}

type Bridge interface {
	Start(context.Context, Callbacks) error
	Wait(context.Context) error
	Close() error
}

type Clock interface {
	Now() time.Time
}

type GenerationState struct {
	Generation Generation
	Ready      bool
}

type Options struct {
	Bridge      Bridge
	Rules       ExecutableMatcher
	Dialer      Dialer
	DNS         netip.Addr
	QueueSize   int
	IdleTimeout time.Duration
	Clock       Clock
}

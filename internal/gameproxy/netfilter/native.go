package netfilter

import (
	"context"
	"net/netip"

	"bork/internal/gameproxy/intercept"
)

type nativeDirection uint8
type nativeAddressFamily uint8
type nativeProtocol uint8
type nativeFilteringFlag uint32

const (
	nativeDirectionOutbound nativeDirection     = 2
	nativeAddressFamilyIPv4 nativeAddressFamily = 2
	nativeProtocolTCP       nativeProtocol      = 6
	nativeProtocolUDP       nativeProtocol      = 17
	nativeFlagFilter        nativeFilteringFlag = 2
	nativeFlagOffline       nativeFilteringFlag = 8
)

type nativeRule struct {
	direction      nativeDirection
	family         nativeAddressFamily
	protocol       nativeProtocol
	flags          nativeFilteringFlag
	executablePath string
}

type nativeTCPConnectedEvent struct {
	ID             intercept.NativeID
	PID            intercept.ProcessID
	ExecutablePath string
	Local          netip.AddrPort
	Remote         netip.AddrPort
}

type nativeUDPCreatedEvent struct {
	ID             intercept.NativeID
	PID            intercept.ProcessID
	ExecutablePath string
	Local          netip.AddrPort
}

type nativeUDPSendEvent struct {
	ID      intercept.NativeID
	Local   netip.AddrPort
	Remote  netip.AddrPort
	Payload []byte
}

type nativeCallbackSink interface {
	nativeCallbackSink()
	tcpConnected(nativeTCPConnectedEvent)
	tcpSend(intercept.NativeID, []byte)
	tcpClosed(intercept.NativeID)
	udpCreated(nativeUDPCreatedEvent)
	udpSend(nativeUDPSendEvent)
	udpClosed(intercept.NativeID)
}

type nativeBackend interface {
	Start(context.Context, nativeCallbackSink, []nativeRule) error
	Wait(context.Context) error
	PostTCPReceive(context.Context, intercept.NativeID, []byte) error
	CloseTCP(intercept.NativeID) error
	PostUDPReceive(context.Context, intercept.NativeID, netip.AddrPort, []byte) error
	SuspendUDP(intercept.NativeID) error
	Close() error
}

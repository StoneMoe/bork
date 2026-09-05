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
	nativeDirectionOutbound           nativeDirection     = 2
	nativeAddressFamilyIPv4           nativeAddressFamily = 2
	nativeProtocolTCP                 nativeProtocol      = 6
	nativeProtocolUDP                 nativeProtocol      = 17
	nativeFlagAllow                   nativeFilteringFlag = 0
	nativeFlagFilter                  nativeFilteringFlag = 2
	nativeFlagOffline                 nativeFilteringFlag = 8
	nativeFlagIndicateConnectRequests nativeFilteringFlag = 16
)

type nativeRule struct {
	direction      nativeDirection
	family         nativeAddressFamily
	protocol       nativeProtocol
	flags          nativeFilteringFlag
	processID      intercept.ProcessID
	executablePath string
}

type nativeTCPConnectRequestEvent struct {
	ID             intercept.NativeID
	PID            intercept.ProcessID
	ExecutablePath string
	Local          netip.AddrPort
	Remote         netip.AddrPort
}

// nativeTCPConnectedEvent remains for the packet-filter adapter tests. Redirect mode does not
// admit TCP streams from tcpConnected callbacks.
type nativeTCPConnectedEvent = nativeTCPConnectRequestEvent

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
	Options nativeUDPOptions
}

type nativeUDPOptions struct {
	flags uint32
	data  []byte
}

type nativeCallbackSink interface {
	nativeCallbackSink()
	tcpConnectRequest(nativeTCPConnectRequestEvent) uint16
	tcpConnected(nativeTCPConnectedEvent)
	tcpSend(intercept.NativeID, []byte)
	tcpClosed(intercept.NativeID)
	udpCreated(nativeUDPCreatedEvent)
	udpSend(nativeUDPSendEvent)
	udpClosed(intercept.NativeID)
	endpointError(*NativeCallbackError)
}

type nativeBackend interface {
	Start(context.Context, nativeCallbackSink, []nativeRule) error
	UpdateRules(context.Context, []nativeRule) error
	Wait(context.Context) error
	PostTCPReceive(context.Context, intercept.NativeID, []byte) error
	CloseTCP(intercept.NativeID) error
	PostUDPReceive(context.Context, intercept.NativeID, netip.AddrPort, []byte, nativeUDPOptions) error
	SuspendUDP(intercept.NativeID) error
	Close() error
}

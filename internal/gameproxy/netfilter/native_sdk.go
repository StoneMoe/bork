package netfilter

import (
	"net/netip"

	"bork/internal/gameproxy/intercept"
)

type nativeStartResult struct {
	status        nativeStatus
	systemError   uint32
	initAttempted bool
	initSucceeded bool
}

type nativeSDK interface {
	Start(uint64, nativeConfig, []nativeRule) nativeStartResult
	DisableCallbacks(uint64)
	DrainCallbacks(uint64)
	Shutdown(uint64, bool)
	PostTCPReceive(uint64, intercept.NativeID, []byte) nativeStatus
	CloseTCP(uint64, intercept.NativeID) nativeStatus
	PostUDPReceive(uint64, intercept.NativeID, netip.AddrPort, []byte) nativeStatus
	SuspendUDP(uint64, intercept.NativeID) nativeStatus
}

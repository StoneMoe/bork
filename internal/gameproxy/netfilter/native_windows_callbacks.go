//go:build windows && amd64 && cgo && netfilter_sdk

package netfilter

/*
#include "nfshim.h"
*/
import "C"

import (
	"net/netip"

	"bork/internal/gameproxy/intercept"
)

func nativeIPv4(a, b, c, d C.uint8_t, port C.uint16_t) netip.AddrPort {
	address := netip.AddrFrom4([4]byte{byte(a), byte(b), byte(c), byte(d)})
	return netip.AddrPortFrom(address, uint16(port))
}

//export goNFTCPConnected
func goNFTCPConnected(token, id C.uint64_t, pid C.uint32_t, path *C.char, pathLength C.int,
	localA, localB, localC, localD C.uint8_t, localPort C.uint16_t,
	remoteA, remoteB, remoteC, remoteD C.uint8_t, remotePort C.uint16_t,
) {
	dispatchNativeCallback(uint64(token), nativeEventTCPConnected, func(owner *sdkNativeBackend) {
		owner.deliverTCPConnected(nativeTCPConnectedEvent{
			ID:             intercept.NativeID(id),
			PID:            intercept.ProcessID(pid),
			ExecutablePath: C.GoStringN(path, pathLength),
			Local:          nativeIPv4(localA, localB, localC, localD, localPort),
			Remote:         nativeIPv4(remoteA, remoteB, remoteC, remoteD, remotePort),
		})
	})
}

//export goNFTCPSend
func goNFTCPSend(token, id C.uint64_t, data *C.char, length C.int) {
	dispatchNativeCallback(uint64(token), nativeEventTCPSend, func(owner *sdkNativeBackend) {
		owner.deliverTCPSend(intercept.NativeID(id), []byte(C.GoStringN(data, length)))
	})
}

//export goNFTCPClosed
func goNFTCPClosed(token, id C.uint64_t) {
	dispatchNativeCallback(uint64(token), nativeEventTCPClosed, func(owner *sdkNativeBackend) {
		owner.deliverTCPClosed(intercept.NativeID(id))
	})
}

//export goNFUDPCreated
func goNFUDPCreated(token, id C.uint64_t, pid C.uint32_t, path *C.char, pathLength C.int,
	localA, localB, localC, localD C.uint8_t, localPort C.uint16_t,
) {
	dispatchNativeCallback(uint64(token), nativeEventUDPCreated, func(owner *sdkNativeBackend) {
		owner.deliverUDPCreated(nativeUDPCreatedEvent{
			ID:             intercept.NativeID(id),
			PID:            intercept.ProcessID(pid),
			ExecutablePath: C.GoStringN(path, pathLength),
			Local:          nativeIPv4(localA, localB, localC, localD, localPort),
		})
	})
}

//export goNFUDPSend
func goNFUDPSend(token, id C.uint64_t,
	localA, localB, localC, localD C.uint8_t, localPort C.uint16_t,
	remoteA, remoteB, remoteC, remoteD C.uint8_t, remotePort C.uint16_t,
	data *C.char, length C.int,
) {
	dispatchNativeCallback(uint64(token), nativeEventUDPSend, func(owner *sdkNativeBackend) {
		owner.deliverUDPSend(nativeUDPSendEvent{
			ID:      intercept.NativeID(id),
			Local:   nativeIPv4(localA, localB, localC, localD, localPort),
			Remote:  nativeIPv4(remoteA, remoteB, remoteC, remoteD, remotePort),
			Payload: []byte(C.GoStringN(data, length)),
		})
	})
}

//export goNFUDPClosed
func goNFUDPClosed(token, id C.uint64_t) {
	dispatchNativeCallback(uint64(token), nativeEventUDPClosed, func(owner *sdkNativeBackend) {
		owner.deliverUDPClosed(intercept.NativeID(id))
	})
}

//export goNFFatal
func goNFFatal(token C.uint64_t, event C.int, id C.uint64_t, reason C.int,
	status, cleanupStatus C.int32_t,
) {
	dispatchNativeCallback(uint64(token), nativeEvent(event), func(owner *sdkNativeBackend) {
		cleanup := nativeStatus(cleanupStatus)
		if cleanup == nativeStatusInvalidEndpointID {
			cleanup = nativeStatusSuccess
		}
		owner.reportFatal(&NativeCallbackError{
			Event:         nativeEvent(event),
			ID:            intercept.NativeID(id),
			Reason:        nativeFailureReason(reason),
			Status:        nativeStatus(status),
			CleanupStatus: cleanup,
		})
	})
}

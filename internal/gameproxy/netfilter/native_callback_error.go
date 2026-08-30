package netfilter

import (
	"fmt"

	"bork/internal/gameproxy/intercept"
)

type nativeEvent uint8
type nativeFailureReason uint8

const (
	nativeEventTCPConnectRequest nativeEvent = iota + 1
	nativeEventTCPConnected
	nativeEventTCPReceive
	nativeEventTCPSend
	nativeEventUDPCreated
	nativeEventUDPConnectRequest
	nativeEventUDPReceive
	nativeEventUDPSend
	nativeEventTCPClosed
	nativeEventUDPClosed
)

const (
	nativeReasonUnsupported nativeFailureReason = iota + 1
	nativeReasonIncomingTCP
	nativeReasonIPv6
	nativeReasonMalformed
	nativeReasonProcessPath
	nativeReasonUDPQuery
)

func (event nativeEvent) String() string {
	switch event {
	case nativeEventTCPConnectRequest:
		return "TCP connect request"
	case nativeEventTCPConnected:
		return "TCP connected"
	case nativeEventTCPReceive:
		return "TCP receive"
	case nativeEventTCPSend:
		return "TCP send"
	case nativeEventUDPCreated:
		return "UDP created"
	case nativeEventUDPConnectRequest:
		return "UDP connect request"
	case nativeEventUDPReceive:
		return "UDP receive"
	case nativeEventUDPSend:
		return "UDP send"
	case nativeEventTCPClosed:
		return "TCP closed"
	case nativeEventUDPClosed:
		return "UDP closed"
	default:
		return fmt.Sprintf("event(%d)", event)
	}
}

type NativeCallbackError struct {
	Event         nativeEvent
	ID            intercept.NativeID
	Reason        nativeFailureReason
	Status        nativeStatus
	CleanupStatus nativeStatus
	Cause         error
}

func (failure *NativeCallbackError) Error() string {
	if failure.Cause != nil {
		return fmt.Sprintf("netfilter: %s callback: %v", failure.Event, failure.Cause)
	}
	return fmt.Sprintf("netfilter: %s callback for endpoint %d failed: reason %d, status %d, cleanup status %d",
		failure.Event, failure.ID, failure.Reason, failure.Status, failure.CleanupStatus)
}

func (failure *NativeCallbackError) Unwrap() error { return failure.Cause }

type NativeStartError struct {
	Operation   nativeOperation
	Status      nativeStatus
	SystemError uint32
}

func (failure *NativeStartError) Error() string {
	if failure.SystemError != 0 {
		return fmt.Sprintf("netfilter: %s: SDK status %d, Windows error %d",
			failure.Operation, failure.Status, failure.SystemError)
	}
	return fmt.Sprintf("netfilter: %s: SDK status %d", failure.Operation, failure.Status)
}

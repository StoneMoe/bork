package netfilter

import (
	"fmt"

	"bork/internal/gameproxy/intercept"
)

type nativeEvent uint8
type nativeFailureReason uint8

func (event nativeEvent) protocol() nativeProtocol {
	switch event {
	case nativeEventTCPConnectRequest, nativeEventTCPConnected, nativeEventTCPReceive,
		nativeEventTCPSend, nativeEventTCPClosed:
		return nativeProtocolTCP
	case nativeEventUDPCreated, nativeEventUDPConnectRequest, nativeEventUDPReceive,
		nativeEventUDPSend, nativeEventUDPClosed:
		return nativeProtocolUDP
	default:
		return 0
	}
}

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

func (reason nativeFailureReason) String() string {
	switch reason {
	case nativeReasonUnsupported:
		return "unsupported callback"
	case nativeReasonIncomingTCP:
		return "incoming TCP"
	case nativeReasonIPv6:
		return "IPv6"
	case nativeReasonMalformed:
		return "malformed callback data"
	case nativeReasonProcessPath:
		return "process path"
	case nativeReasonUDPQuery:
		return "UDP endpoint query"
	case nativeReasonPostReceive:
		return "post receive"
	case nativeReasonPostSend:
		return "post send"
	case nativeReasonSelfInterception:
		return "self interception"
	case nativeReasonLocalRoute:
		return "local route"
	default:
		return fmt.Sprintf("reason(%d)", reason)
	}
}

const (
	nativeReasonUnsupported nativeFailureReason = iota + 1
	nativeReasonIncomingTCP
	nativeReasonIPv6
	nativeReasonMalformed
	nativeReasonProcessPath
	nativeReasonUDPQuery
	nativeReasonPostReceive
	nativeReasonPostSend
	nativeReasonSelfInterception
	nativeReasonLocalRoute
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
	return fmt.Sprintf("netfilter: %s callback for endpoint %d failed: reason %s (%d), status %d, cleanup status %d",
		failure.Event, failure.ID, failure.Reason, failure.Reason, failure.Status, failure.CleanupStatus)
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

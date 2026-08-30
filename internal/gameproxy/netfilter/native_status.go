package netfilter

import "fmt"

type nativeStatus int32

const (
	nativeStatusSuccess           nativeStatus = 0
	nativeStatusFail              nativeStatus = -1
	nativeStatusInvalidEndpointID nativeStatus = -2
	nativeStatusNotInitialized    nativeStatus = -3
	nativeStatusIOError           nativeStatus = -4
	nativeStatusRebootRequired    nativeStatus = -5
)

type nativeOperation uint8

const (
	nativeOperationInit nativeOperation = iota
	nativeOperationSetRules
	nativeOperationPostTCPReceive
	nativeOperationCloseTCP
	nativeOperationPostUDPReceive
	nativeOperationSuspendUDP
	nativeOperationGetUDPInfo
)

func (operation nativeOperation) String() string {
	switch operation {
	case nativeOperationInit:
		return "initialize"
	case nativeOperationSetRules:
		return "set rules"
	case nativeOperationPostTCPReceive:
		return "post TCP receive"
	case nativeOperationCloseTCP:
		return "close TCP"
	case nativeOperationPostUDPReceive:
		return "post UDP receive"
	case nativeOperationSuspendUDP:
		return "suspend UDP"
	case nativeOperationGetUDPInfo:
		return "query UDP connection"
	default:
		return fmt.Sprintf("operation(%d)", operation)
	}
}

type NativeStatusError struct {
	Operation nativeOperation
	Status    nativeStatus
}

func (failure *NativeStatusError) Error() string {
	return fmt.Sprintf("netfilter: %s: SDK status %d", failure.Operation, failure.Status)
}

func nativeStatusError(operation nativeOperation, status nativeStatus) error {
	if status == nativeStatusSuccess {
		return nil
	}
	if status == nativeStatusInvalidEndpointID &&
		(operation == nativeOperationCloseTCP || operation == nativeOperationSuspendUDP) {
		return nil
	}
	return &NativeStatusError{Operation: operation, Status: status}
}

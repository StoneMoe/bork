package intercept

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidOptions  = errors.New("intercept: invalid options")
	ErrNotReady        = errors.New("intercept: generation not ready")
	ErrStaleGeneration = errors.New("intercept: stale generation")
	ErrUnselected      = errors.New("intercept: executable is not selected")
	ErrInvalidFlow     = errors.New("intercept: invalid flow")
	ErrDuplicateFlow   = errors.New("intercept: duplicate native flow")
	ErrQueueFull       = errors.New("intercept: packet queue full")
	ErrDial            = errors.New("intercept: dial failed")
	ErrPacket          = errors.New("intercept: packet relay failed")
	ErrIdle            = errors.New("intercept: UDP endpoint idle")
	ErrRelayStarted    = errors.New("intercept: relay already started")
	ErrBridgeFatal     = errors.New("intercept: bridge failed")
	ErrBridgeStopped   = errors.New("intercept: bridge stopped unexpectedly")
)

type FlowError struct {
	NativeID  NativeID
	Operation string
	Cause     error
}

func (failure *FlowError) Error() string {
	return fmt.Sprintf("intercept flow %d %s: %v", failure.NativeID, failure.Operation, failure.Cause)
}

func (failure *FlowError) Unwrap() error { return failure.Cause }

type BridgeError struct {
	Cause error
}

func (failure *BridgeError) Error() string { return fmt.Sprintf("intercept bridge: %v", failure.Cause) }
func (failure *BridgeError) Unwrap() error { return errors.Join(ErrBridgeFatal, failure.Cause) }

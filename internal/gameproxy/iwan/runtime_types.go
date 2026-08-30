package iwan

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

var (
	ErrInvalidOptions        = errors.New("iwan: invalid runtime options")
	ErrNotReady              = errors.New("iwan: runtime is not ready")
	ErrAuthRejected          = errors.New("iwan: authentication rejected")
	ErrProtocolDowngrade     = errors.New("iwan: XOR protocol downgrade")
	ErrProtocolConfiguration = errors.New("iwan: protocol configuration failure")
	ErrAuthTimeout           = errors.New("iwan: authentication timeout")
	ErrInactive              = errors.New("iwan: session inactive")
	ErrPeerClosed            = errors.New("iwan: peer closed session")
	ErrSocketFailure         = errors.New("iwan: UDP socket failure")
	ErrStackFailure          = errors.New("iwan: network stack failure")
)

type FailureClass uint8

const (
	FailureTransient FailureClass = iota + 1
	FailureTerminal
)

type RuntimeError struct {
	Class     FailureClass
	Operation string
	Cause     error
}

func (failure *RuntimeError) Error() string {
	return fmt.Sprintf("iwan %s: %v", failure.Operation, failure.Cause)
}

func (failure *RuntimeError) Unwrap() error { return failure.Cause }

type Node struct {
	Server   string
	Port     uint16
	Username string
	Password string
	MTU      uint16
}

type Options struct {
	Node      Node
	QueueSize int
}

type State string

const (
	StateStopped        State = "stopped"
	StateConnecting     State = "connecting"
	StateAuthenticating State = "authenticating"
	StateReady          State = "ready"
	StateRetrying       State = "retrying"
	StateFailed         State = "failed"
)

type Status struct {
	State      State
	Generation uint64
	Address    netip.Addr
	MTU        uint16
	Err        error
}

type runtimeTimings struct {
	openRetry    time.Duration
	authTimeout  time.Duration
	echoInterval time.Duration
	liveness     time.Duration
	restartDelay time.Duration
}

func defaultRuntimeTimings() runtimeTimings {
	return runtimeTimings{
		openRetry: RetryInterval, authTimeout: AuthTimeout,
		echoInterval: RetryInterval, liveness: EstablishedTimeout,
		restartDelay: time.Second,
	}
}

func normalizeOptions(options Options) (Options, Credentials, error) {
	options.Node.Server = strings.TrimSpace(options.Node.Server)
	if options.Node.Server == "" {
		return Options{}, Credentials{}, fmt.Errorf("server: %w", ErrInvalidOptions)
	}
	if options.Node.Port == 0 {
		options.Node.Port = DefaultPort
	}
	if options.Node.MTU == 0 {
		options.Node.MTU = DefaultMTU
	}
	if options.Node.MTU < MinMTU || options.Node.MTU > MaxMTU {
		return Options{}, Credentials{}, fmt.Errorf("MTU %d: %w", options.Node.MTU, ErrInvalidOptions)
	}
	if options.QueueSize == 0 {
		options.QueueSize = 256
	}
	if options.QueueSize < 1 {
		return Options{}, Credentials{}, fmt.Errorf("queue size %d: %w", options.QueueSize, ErrInvalidOptions)
	}
	credentials, err := NewCredentials(options.Node.Username, options.Node.Password)
	if err != nil {
		return Options{}, Credentials{}, fmt.Errorf("credentials: %w", ErrInvalidOptions)
	}
	return options, credentials, nil
}

func terminalFailure(operation string, cause error) error {
	return &RuntimeError{Class: FailureTerminal, Operation: operation, Cause: cause}
}

func transientFailure(operation string, cause error) error {
	return &RuntimeError{Class: FailureTransient, Operation: operation, Cause: cause}
}

func isTerminalFailure(err error) bool {
	var failure *RuntimeError
	return errors.As(err, &failure) && failure.Class == FailureTerminal
}

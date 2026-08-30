package gameproxy

import (
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"

	"bork/internal/gameproxy/iwan"
)

var (
	ErrUnsupported       = errors.New("gameproxy: unsupported")
	ErrActive            = errors.New("gameproxy: already active")
	ErrInvalidStartInput = errors.New("gameproxy: invalid start input")
	ErrNoExecutables     = errors.New("gameproxy: no executable rules")
	ErrSupervisorStopped = errors.New("gameproxy: iwan supervisor stopped unexpectedly")
)

type State string

const (
	StateUnsupported  State = "unsupported"
	StateInactive     State = "inactive"
	StateStarting     State = "starting"
	StateRunning      State = "running"
	StateReconnecting State = "reconnecting"
	StateStopping     State = "stopping"
	StateFailed       State = "failed"
)

type Status struct {
	Supported       bool   `json:"supported"`
	State           State  `json:"state"`
	Generation      uint64 `json:"generation"`
	ExecutableCount int    `json:"executableCount"`
	Directory       string `json:"directory"`
	Error           string `json:"error,omitempty"`
}

type StartInput struct {
	Node      iwan.Node
	Directory string
	DNS       netip.Addr
}

func normalizeStartInput(input StartInput) (StartInput, error) {
	input.Directory = strings.TrimSpace(input.Directory)
	input.Node.Server = strings.TrimSpace(input.Node.Server)
	if input.Directory == "" {
		return StartInput{}, fmt.Errorf("directory: %w", ErrInvalidStartInput)
	}
	if input.Node.Server == "" {
		return StartInput{}, fmt.Errorf("iwan server: %w", ErrInvalidStartInput)
	}
	if input.Node.MTU != 0 && (input.Node.MTU < iwan.MinMTU || input.Node.MTU > iwan.MaxMTU) {
		return StartInput{}, fmt.Errorf("iwan MTU: %w", ErrInvalidStartInput)
	}
	if _, err := iwan.NewCredentials(input.Node.Username, input.Node.Password); err != nil {
		return StartInput{}, fmt.Errorf("iwan credentials: %w", ErrInvalidStartInput)
	}
	if !input.DNS.Is4() || input.DNS.Zone() != "" {
		return StartInput{}, fmt.Errorf("DNS: %w", ErrInvalidStartInput)
	}
	input.Directory = filepath.Clean(input.Directory)
	return input, nil
}

package gameproxy

import (
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"

	"bork/internal/gameproxy/intercept"
	"bork/internal/gameproxy/iwan"
	"bork/internal/gameproxy/netstack"
)

var (
	ErrUnsupported       = errors.New("gameproxy: unsupported")
	ErrActive            = errors.New("gameproxy: already active")
	ErrInvalidStartInput = errors.New("gameproxy: invalid start input")
	ErrNoExecutables     = errors.New("gameproxy: no executable rules")
	ErrSupervisorStopped = errors.New("gameproxy: iwan supervisor stopped unexpectedly")
	ErrNotActive         = errors.New("gameproxy: not active")
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
	Supported       bool              `json:"supported"`
	State           State             `json:"state"`
	Generation      uint64            `json:"generation"`
	ExecutableCount int               `json:"executableCount"`
	Directories     []string          `json:"directories"`
	Events          []ConnectionEvent `json:"events"`
	Error           string            `json:"error,omitempty"`
	Traffic         TrafficStats      `json:"traffic"`
}

type ConnectionEvent struct {
	At      string `json:"at"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

type TrafficStats = intercept.TrafficStats

type StartInput struct {
	Node        iwan.Node
	Directories []string
	DNS         netip.Addr
}

func normalizeStartInput(input StartInput) (StartInput, error) {
	input.Node.Server = strings.TrimSpace(input.Node.Server)
	input.Node.Username = strings.TrimSpace(input.Node.Username)
	input.Directories = normalizeDirectories(input.Directories)
	if len(input.Directories) == 0 {
		return StartInput{}, fmt.Errorf("directories: %w", ErrInvalidStartInput)
	}
	if input.Node.Server == "" {
		return StartInput{}, fmt.Errorf("iwan server: %w", ErrInvalidStartInput)
	}
	if input.Node.MTU != 0 && (input.Node.MTU < netstack.MinMTU || input.Node.MTU > iwan.MaxMTU) {
		return StartInput{}, fmt.Errorf("iwan MTU: %w", ErrInvalidStartInput)
	}
	if _, err := iwan.NewCredentials(input.Node.Username, input.Node.Password); err != nil {
		return StartInput{}, fmt.Errorf("iwan credentials: %w", ErrInvalidStartInput)
	}
	if !input.DNS.Is4() || input.DNS.Zone() != "" {
		return StartInput{}, fmt.Errorf("DNS: %w", ErrInvalidStartInput)
	}
	return input, nil
}

func normalizeDirectories(directories []string) []string {
	normalized := make([]string, 0, len(directories))
	for _, directory := range directories {
		directory = strings.TrimSpace(directory)
		if directory == "" {
			continue
		}
		directory = filepath.Clean(directory)
		if !slices.Contains(normalized, directory) {
			normalized = append(normalized, directory)
		}
	}
	return normalized
}

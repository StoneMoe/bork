package gameproxy_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"bork/internal/gameproxy"
	"bork/internal/gameproxy/iwan"
)

func TestManager_public_API_reports_unsupported_without_platform_bridge(t *testing.T) {
	// Given
	manager := gameproxy.NewManager(nil)
	input := gameproxy.StartInput{
		Node: iwan.Node{
			Server: "127.0.0.1", Username: "user", Password: "secret",
		},
		Directory: "games",
		DNS:       netip.MustParseAddr("1.1.1.1"),
	}

	// When
	err := manager.Start(context.Background(), input)
	status := manager.Status()

	// Then
	if !errors.Is(err, gameproxy.ErrUnsupported) {
		t.Fatalf("Start error = %v, want ErrUnsupported", err)
	}
	if status.Supported || status.State != gameproxy.StateUnsupported || status.Error != "" {
		t.Fatalf("unsupported status = %#v", status)
	}
}

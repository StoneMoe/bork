//go:build game_proxy && (!windows || !amd64)

package app

import (
	"errors"
	"testing"

	"bork/internal/gameproxy/netfilter"
)

func TestGetGameProxyLicense_without_SDK_needs_no_startup(t *testing.T) {
	application := &App{}
	license, err := application.GetGameProxyLicense()
	if license != "" || !errors.Is(err, netfilter.ErrUnsupported) {
		t.Fatalf("GetGameProxyLicense() = %q, %v; want empty license and ErrUnsupported", license, err)
	}
}

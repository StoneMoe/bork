//go:build windows && !wintun_embed

package peer

import (
	"context"
	"strings"
	"testing"
)

func TestWintunDevelopmentBuildError(t *testing.T) {
	err := prepareWintun(context.Background())
	if err == nil || !strings.Contains(err.Error(), "wintun_embed") {
		t.Fatalf("prepareWintun() error = %v", err)
	}
}

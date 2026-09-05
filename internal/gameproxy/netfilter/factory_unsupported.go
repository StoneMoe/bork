//go:build game_proxy && (!windows || !amd64 || !cgo)

package netfilter

import (
	"context"

	"bork/internal/gameproxy/intercept"
)

type Factory struct{}

func NewFactory() *Factory { return &Factory{} }

func (*Factory) Supported() bool { return false }

func (*Factory) EnsureAvailable(context.Context) error { return ErrUnsupported }

func (*Factory) New(context.Context, []string) (intercept.Bridge, error) {
	return nil, ErrUnsupported
}

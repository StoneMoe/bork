//go:build windows && !wintun_embed

package peer

import (
	"context"
	"errors"
)

func prepareWintun(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("Windows Virtual LAN is unavailable in this development build; rebuild with -tags wintun_embed (make build or make dev)")
}

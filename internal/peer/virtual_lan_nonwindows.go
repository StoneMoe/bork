//go:build !windows

package peer

import "context"

func prepareVirtualLANPlatform(context.Context) error { return nil }

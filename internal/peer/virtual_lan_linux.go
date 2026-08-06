//go:build linux

package peer

import (
	"context"
	"fmt"
	"net/netip"
)

func virtualLANPlatformName(hash [32]byte) string { return fmt.Sprintf("bork%x", hash[:4]) }

func configureVirtualLANPlatform(ctx context.Context, name string, address netip.Addr, mtu int) (func() error, error) {
	if err := runVirtualLANCommand(ctx, "ip", "addr", "replace", address.String()+"/10", "dev", name); err != nil {
		return nil, fmt.Errorf("configure TUN IPv4 address with ip (CAP_NET_ADMIN required): %w", err)
	}
	if err := runVirtualLANCommand(ctx, "ip", "link", "set", "dev", name, "mtu", fmt.Sprint(mtu), "up"); err != nil {
		_ = runVirtualLANCleanupCommand("ip", "addr", "del", address.String()+"/10", "dev", name)
		return nil, fmt.Errorf("bring up TUN with ip (CAP_NET_ADMIN required): %w", err)
	}
	return func() error {
		if err := runVirtualLANCleanupCommand("ip", "addr", "del", address.String()+"/10", "dev", name); err != nil {
			return fmt.Errorf("remove TUN IPv4 address with ip: %w", err)
		}
		return nil
	}, nil
}

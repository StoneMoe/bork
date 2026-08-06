//go:build darwin

package peer

import (
	"context"
	"fmt"
	"net/netip"
)

func virtualLANPlatformName(hash [32]byte) string {
	return "utun"
}

func configureVirtualLANPlatform(ctx context.Context, name string, address netip.Addr, mtu int) (func() error, error) {
	if err := runVirtualLANCommand(ctx, "ifconfig", name, "inet", address.String(), address.String(), "netmask", "255.192.0.0", "mtu", fmt.Sprint(mtu), "up"); err != nil {
		return nil, fmt.Errorf("configure TUN with ifconfig (root required): %w", err)
	}
	if err := runVirtualLANCommand(ctx, "route", "-n", "add", "-net", "100.64.0.0/10", "-interface", name); err != nil {
		_ = runVirtualLANCleanupCommand("ifconfig", name, "inet", address.String(), "delete")
		return nil, fmt.Errorf("configure overlay route (root required): %w", err)
	}
	return func() error {
		routeErr := runVirtualLANCleanupCommand("route", "-n", "delete", "-net", "100.64.0.0/10", "-interface", name)
		addressErr := runVirtualLANCleanupCommand("ifconfig", name, "inet", address.String(), "delete")
		if routeErr != nil {
			return fmt.Errorf("remove overlay route: %w", routeErr)
		}
		if addressErr != nil {
			return fmt.Errorf("remove TUN IPv4 address: %w", addressErr)
		}
		return nil
	}, nil
}

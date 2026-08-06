//go:build windows

package peer

import (
	"context"
	"fmt"
	"net/netip"
)

func virtualLANPlatformName(hash [32]byte) string { return fmt.Sprintf("bork-%x", hash[:4]) }

func prepareVirtualLANPlatform(ctx context.Context) error { return prepareWintun(ctx) }

func configureVirtualLANPlatform(ctx context.Context, name string, address netip.Addr, mtu int) (func() error, error) {
	if err := runVirtualLANCommand(ctx, "netsh", "interface", "ipv4", "set", "address", "name="+name, "source=static", "address="+address.String(), "mask=255.192.0.0", "gateway=none", "store=active"); err != nil {
		return nil, fmt.Errorf("configure TUN IPv4 address with netsh (run as Administrator): %w", err)
	}
	if err := runVirtualLANCommand(ctx, "netsh", "interface", "ipv4", "set", "subinterface", name, fmt.Sprintf("mtu=%d", mtu), "store=active"); err != nil {
		_ = removeVirtualLANPlatformAddress(name, address)
		return nil, fmt.Errorf("configure TUN MTU with netsh (run as Administrator): %w", err)
	}
	return func() error {
		return removeVirtualLANPlatformAddress(name, address)
	}, nil
}

func removeVirtualLANPlatformAddress(name string, address netip.Addr) error {
	err := runVirtualLANCleanupCommand("netsh", "interface", "ipv4", "delete", "address", "name="+name, "address="+address.String(), "store=active")
	if err != nil {
		return fmt.Errorf("remove TUN IPv4 address with netsh: %w", err)
	}
	return nil
}

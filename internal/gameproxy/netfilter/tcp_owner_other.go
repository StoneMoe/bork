//go:build !windows

package netfilter

import (
	"net/netip"

	"bork/internal/gameproxy/intercept"
)

func tcpSocketOwner(netip.AddrPort, netip.AddrPort) (intercept.ProcessID, error) {
	return 0, ErrUnsupported
}

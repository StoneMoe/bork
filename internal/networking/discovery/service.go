package discovery

import (
	"context"
	"net/netip"
)

type Service interface {
	Run(context.Context, [16]byte, netip.AddrPort, chan<- Hint) error
}

func DefaultServices() []Service {
	return []Service{newLocalDiscovery(), newMDNSDiscovery()}
}

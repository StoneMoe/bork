package link

import (
	"errors"
	"net/netip"
)

type Path struct {
	address netip.AddrPort
}

func NewPath(address netip.AddrPort) (Path, error) {
	address = netip.AddrPortFrom(address.Addr().Unmap(), address.Port())
	if !address.IsValid() || address.Port() == 0 {
		return Path{}, errors.New("network path address is invalid")
	}
	return Path{address: address}, nil
}

func (p Path) Address() netip.AddrPort { return p.address }
func (p Path) IsValid() bool           { return p.address.IsValid() && p.address.Port() != 0 }

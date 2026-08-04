package peer

import (
	"errors"
	"net/netip"
)

type Path struct {
	address      netip.AddrPort
	intermediary [32]byte
	target       [32]byte
}

func NewPath(address netip.AddrPort) (Path, error) {
	address = netip.AddrPortFrom(address.Addr().Unmap(), address.Port())
	if !address.IsValid() || address.Port() == 0 {
		return Path{}, errors.New("network path address is invalid")
	}
	return Path{address: address}, nil
}

func NewBridgePath(nextHop netip.AddrPort, intermediary, target [32]byte) (Path, error) {
	path, err := NewPath(nextHop)
	if err != nil || intermediary == ([32]byte{}) || target == ([32]byte{}) || intermediary == target {
		return Path{}, errors.New("bridge path is invalid")
	}
	path.intermediary = intermediary
	path.target = target
	return path, nil
}

func (p Path) Address() netip.AddrPort { return p.address }
func (p Path) IsDirect() bool          { return p.intermediary == ([32]byte{}) && p.target == ([32]byte{}) }
func (p Path) Intermediary() [32]byte  { return p.intermediary }
func (p Path) Target() [32]byte        { return p.target }

func (p Path) IsValid() bool {
	if !p.address.IsValid() || p.address.Port() == 0 {
		return false
	}
	return p.IsDirect() || (p.intermediary != ([32]byte{}) && p.target != ([32]byte{}) && p.intermediary != p.target)
}

func (p Path) SameRoute(other Path) bool {
	if p.IsDirect() || other.IsDirect() {
		return p.IsDirect() && other.IsDirect() && p.address == other.address
	}
	return p.intermediary == other.intermediary && p.target == other.target
}

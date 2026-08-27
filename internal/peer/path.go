package peer

import (
	"errors"
	"net/netip"

	"bork/internal/identity"
)

type Path struct {
	address      netip.AddrPort
	intermediary identity.PeerID
	target       identity.PeerID
}

func NewPath(address netip.AddrPort) (Path, error) {
	address = netip.AddrPortFrom(address.Addr().Unmap(), address.Port())
	if !address.IsValid() || address.Port() == 0 {
		return Path{}, errors.New("network path address is invalid")
	}
	return Path{address: address}, nil
}

func NewBridgePath(nextHop netip.AddrPort, intermediary, target identity.PeerID) (Path, error) {
	path, err := NewPath(nextHop)
	if err != nil || intermediary.IsZero() || target.IsZero() || intermediary == target {
		return Path{}, errors.New("bridge path is invalid")
	}
	path.intermediary = intermediary
	path.target = target
	return path, nil
}

func (p Path) Address() netip.AddrPort       { return p.address }
func (p Path) IsDirect() bool                { return p.intermediary.IsZero() && p.target.IsZero() }
func (p Path) Intermediary() identity.PeerID { return p.intermediary }
func (p Path) Target() identity.PeerID       { return p.target }

func (p Path) IsValid() bool {
	if !p.address.IsValid() || p.address.Port() == 0 {
		return false
	}
	return p.IsDirect() || (!p.intermediary.IsZero() && !p.target.IsZero() && p.intermediary != p.target)
}

func (p Path) SameRoute(other Path) bool {
	if p.IsDirect() || other.IsDirect() {
		return p.IsDirect() && other.IsDirect() && p.address == other.address
	}
	return p.intermediary == other.intermediary && p.target == other.target
}

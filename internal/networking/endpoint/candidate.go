package endpoint

import (
	"net"
	"net/netip"
	"sort"
)

type CandidateType string

const (
	CandidateNIC        CandidateType = "nic"
	CandidateSTUN       CandidateType = "stun"
	CandidatePortMapped CandidateType = "port-mapped"
)

type Candidate struct {
	Type      CandidateType `json:"type"`
	Address   string        `json:"address"`
	Family    string        `json:"family"`
	Interface string        `json:"interface,omitempty"`
	Source    string        `json:"source,omitempty"`
}

func nicCandidates(boundAddress netip.Addr, port uint16) ([]Candidate, error) {
	boundAddress = boundAddress.Unmap()
	if boundAddress.IsValid() && !boundAddress.IsUnspecified() {
		if !isUsableNICAddress(boundAddress) {
			return []Candidate{}, nil
		}
		return []Candidate{{
			Type:    CandidateNIC,
			Address: netip.AddrPortFrom(boundAddress, port).String(),
			Family:  addressFamily(boundAddress),
		}}, nil
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	candidates := make([]Candidate, 0, len(interfaces))
	seen := make(map[netip.Addr]struct{})
	for _, networkInterface := range interfaces {
		if networkInterface.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, err := networkInterface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err != nil {
				continue
			}
			parsed, ok := netip.AddrFromSlice(ip)
			if !ok {
				continue
			}
			parsed = parsed.Unmap()
			if boundAddress.Is4() && !parsed.Is4() {
				continue
			}
			if !isUsableNICAddress(parsed) {
				continue
			}
			if _, exists := seen[parsed]; exists {
				continue
			}
			seen[parsed] = struct{}{}
			candidates = append(candidates, Candidate{
				Type:      CandidateNIC,
				Address:   netip.AddrPortFrom(parsed, port).String(),
				Family:    addressFamily(parsed),
				Interface: networkInterface.Name,
			})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Family != candidates[j].Family {
			return candidates[i].Family < candidates[j].Family
		}
		if candidates[i].Interface != candidates[j].Interface {
			return candidates[i].Interface < candidates[j].Interface
		}
		return candidates[i].Address < candidates[j].Address
	})
	return candidates, nil
}

func isUsableNICAddress(address netip.Addr) bool {
	return address.IsValid() &&
		!address.IsUnspecified() &&
		!address.IsLoopback() &&
		!address.IsMulticast() &&
		!address.IsLinkLocalUnicast()
}

func addressFamily(address netip.Addr) string {
	if address.Is4() {
		return "ipv4"
	}
	return "ipv6"
}

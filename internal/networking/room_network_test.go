package networking

import (
	"net/netip"
	"slices"
	"testing"

	"bork/internal/networking/discovery/tracker"
	"bork/internal/networking/endpoint"
)

func TestTrackerAnnounceCandidatesUsesStableSTUNOrder(t *testing.T) {
	snapshot := endpoint.Snapshot{
		Candidates: []endpoint.Candidate{
			{Type: endpoint.CandidateNIC, Address: "[2001:4860:4860::8888]:4000", Family: "ipv6"},
			{Type: endpoint.CandidateNIC, Address: "4.4.4.4:4000", Family: "ipv4"},
			{Type: endpoint.CandidateSTUN, Address: "1.1.1.1:4000", Family: "ipv4", Source: "stun-a"},
			{Type: endpoint.CandidateSTUN, Address: "[2606:4700:4700::1111]:4000", Family: "ipv6", Source: "stun-a"},
			{Type: endpoint.CandidateSTUN, Address: "8.8.8.8:4000", Family: "ipv4", Source: "stun-b"},
			{Type: endpoint.CandidateSTUN, Address: "[2606:4700:4700::1001]:4000", Family: "ipv6", Source: "stun-b"},
			{Type: endpoint.CandidateSTUN, Address: "9.9.9.9:4000", Family: "ipv4", Source: "stun-c"},
		},
		STUN: []endpoint.STUNResult{
			{Server: "stun-a", Family: "ipv4", RTTMillis: 100},
			{Server: "stun-a", Family: "ipv6", RTTMillis: 100},
			{Server: "stun-b", Family: "ipv4", RTTMillis: 1},
			{Server: "stun-b", Family: "ipv6", RTTMillis: 1},
			{Server: "stun-c", Family: "ipv4", RTTMillis: 1},
		},
	}

	candidates := trackerAnnounceCandidates(snapshot)
	want := []tracker.AnnounceCandidate{
		{Address: netip.MustParseAddr("2606:4700:4700::1111"), Port: 4000},
		{Address: netip.MustParseAddr("1.1.1.1"), Port: 4000},
		{Address: netip.MustParseAddr("8.8.8.8"), Port: 4000},
		{Address: netip.MustParseAddr("9.9.9.9"), Port: 4000},
	}
	if !slices.Equal(candidates, want) {
		t.Fatalf("candidates = %v, want stable STUN order %v", candidates, want)
	}
}

func TestTrackerAnnounceCandidatesAcceptsPublicPortMappingWithoutSTUN(t *testing.T) {
	snapshot := endpoint.Snapshot{
		Candidates: []endpoint.Candidate{
			{Type: endpoint.CandidateNIC, Address: "4.4.4.4:4000", Family: "ipv4"},
			{Type: endpoint.CandidatePortMapped, Address: "5.6.7.8:5000", Family: "ipv4"},
		},
	}
	want := []tracker.AnnounceCandidate{
		{Address: netip.MustParseAddr("5.6.7.8"), Port: 5000},
		{Address: netip.MustParseAddr("4.4.4.4"), Port: 4000},
	}
	if candidates := trackerAnnounceCandidates(snapshot); !slices.Equal(candidates, want) {
		t.Fatalf("candidates = %v, want public port mapping before NIC %v", candidates, want)
	}
}

func TestTrackerAnnounceCandidatesUsesPublicNICOrSourceAddressFallback(t *testing.T) {
	withPublicNICs := endpoint.Snapshot{
		ListenAddress: "[::]:4000",
		Candidates: []endpoint.Candidate{
			{Type: endpoint.CandidateNIC, Address: "[fd00::1]:4000", Family: "ipv6"},
			{Type: endpoint.CandidateNIC, Address: "[2001:4860:4860::8888]:4000", Family: "ipv6"},
			{Type: endpoint.CandidateNIC, Address: "1.1.1.1:4000", Family: "ipv4"},
		},
	}
	wantPublic := []tracker.AnnounceCandidate{
		{Address: netip.MustParseAddr("2001:4860:4860::8888"), Port: 4000},
		{Address: netip.MustParseAddr("1.1.1.1"), Port: 4000},
	}
	if candidates := trackerAnnounceCandidates(withPublicNICs); !slices.Equal(candidates, wantPublic) {
		t.Fatalf("candidates = %v, want explicit public candidates %v", candidates, wantPublic)
	}

	withoutPublicCandidate := endpoint.Snapshot{
		ListenAddress: "[::]:4000",
		Candidates: []endpoint.Candidate{
			{Type: endpoint.CandidateNIC, Address: "[fd00::1]:4000", Family: "ipv6"},
		},
	}
	wantFallback := []tracker.AnnounceCandidate{{Port: 4000}}
	if candidates := trackerAnnounceCandidates(withoutPublicCandidate); !slices.Equal(candidates, wantFallback) {
		t.Fatalf("candidates = %v, want request-source fallback %v", candidates, wantFallback)
	}
}

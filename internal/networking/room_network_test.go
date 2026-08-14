package networking

import (
	"net/netip"
	"slices"
	"testing"

	"bork/internal/networking/discovery/tracker"
	"bork/internal/networking/endpoint"
)

func TestTrackerAnnounceCandidatesPrefersIPv6STUNAndKeepsIPv4(t *testing.T) {
	snapshot := endpoint.Snapshot{
		Candidates: []endpoint.Candidate{
			{Type: endpoint.CandidateNIC, Address: "[2001:4860:4860::8888]:4000", Family: "ipv6"},
			{Type: endpoint.CandidateSTUN, Address: "[2606:4700:4700::1111]:4000", Family: "ipv6", Source: "stun-6-1"},
			{Type: endpoint.CandidateSTUN, Address: "[2606:4700:4700::1001]:4000", Family: "ipv6", Source: "stun-6-2"},
			{Type: endpoint.CandidateSTUN, Address: "1.1.1.1:4000", Family: "ipv4", Source: "stun-1"},
			{Type: endpoint.CandidateSTUN, Address: "8.8.8.8:4000", Family: "ipv4", Source: "stun-2"},
			{Type: endpoint.CandidateSTUN, Address: "9.9.9.9:4000", Family: "ipv4", Source: "stun-3"},
		},
		STUN: []endpoint.STUNResult{
			{Server: "stun-6-1", RTTMillis: 1},
			{Server: "stun-6-2", RTTMillis: 2},
			{Server: "stun-1", RTTMillis: 4},
			{Server: "stun-2", RTTMillis: 5},
			{Server: "stun-3", RTTMillis: 6},
		},
	}

	candidates := trackerAnnounceCandidates(snapshot)
	wantIPv6 := tracker.AnnounceCandidate{Address: netip.MustParseAddr("2606:4700:4700::1111"), Port: 4000}
	if len(candidates) != tracker.MaxAnnounceCandidates {
		t.Fatalf("candidate count = %d, want %d", len(candidates), tracker.MaxAnnounceCandidates)
	}
	if candidates[0] != wantIPv6 {
		t.Fatalf("first candidate = %v, want %v", candidates[0], wantIPv6)
	}
	for _, candidate := range candidates[1:] {
		if !candidate.Address.Is4() {
			t.Fatalf("candidate = %v, want IPv4 after the first IPv6 candidate", candidate)
		}
	}
}

func TestTrackerAnnounceCandidatesKeepsObservedFallbackWithPublicIPv6NIC(t *testing.T) {
	snapshot := endpoint.Snapshot{
		ListenAddress: "[::]:4000",
		Candidates: []endpoint.Candidate{
			{Type: endpoint.CandidateNIC, Address: "[fd00::1]:4000", Family: "ipv6"},
			{Type: endpoint.CandidateNIC, Address: "[2001:4860:4860::8888]:4000", Family: "ipv6"},
		},
	}

	candidates := trackerAnnounceCandidates(snapshot)
	want := []tracker.AnnounceCandidate{
		{Address: netip.MustParseAddr("2001:4860:4860::8888"), Port: 4000},
		{Address: netip.IPv4Unspecified(), Port: 4000},
	}
	if !slices.Equal(candidates, want) {
		t.Fatalf("candidates = %v, want IPv6 and observed-address fallback", candidates)
	}

	group, err := tracker.New([]string{
		"https://tracker.example/announce",
		"udp://tracker.example:80/announce",
	}, [20]byte{}, [32]byte{1}, &endpoint.Endpoint{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	group.UpdateCandidates(candidates)
	if statuses := group.Snapshot(); len(statuses) != 3 || statuses[2].Candidate != "0.0.0.0:4000" {
		t.Fatalf("provider candidates = %v, want fallback only on UDP", statuses)
	}
}

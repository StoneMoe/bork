package networking

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"testing"
	"time"

	"bork/internal/networking/discovery/tracker"
	"bork/internal/networking/endpoint"
	"bork/internal/networking/portmap"
)

type lifecyclePortMapper struct {
	mapping portmap.Mapping
	fail    <-chan error
}

func (m *lifecyclePortMapper) Run(ctx context.Context, _ uint16, states chan<- portmap.State) error {
	states <- portmap.State{Mapping: &m.mapping}
	select {
	case err := <-m.fail:
		return err
	case <-ctx.Done():
		return nil
	}
}

func receivePortMappingTestValue[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for port mapping lifecycle")
		var zero T
		return zero
	}
}

func requirePortMappingLane(t *testing.T, lane portMappingLane, wantAddress, wantError string) {
	t.Helper()
	address := ""
	if lane.mapping != nil {
		address = lane.mapping.ExternalAddress.String()
	}
	if address != wantAddress || lane.err != wantError {
		t.Fatalf("port mapping lane = {%q, %q}, want {%q, %q}", address, lane.err, wantAddress, wantError)
	}
}

func TestPortMappingLanesKeepFamiliesIndependent(t *testing.T) {
	failIPv4 := make(chan error, 1)
	ipv4 := &lifecyclePortMapper{mapping: portmap.Mapping{ExternalAddress: netip.MustParseAddrPort("198.51.100.10:5000")}, fail: failIPv4}
	ipv6 := &lifecyclePortMapper{mapping: portmap.Mapping{ExternalAddress: netip.MustParseAddrPort("[2001:db8::10]:6000")}}
	mappings := newPortMappingLanes(ipv4, ipv6)

	mappings.startAvailable(t.Context(), endpoint.Snapshot{
		ListenAddress: "[::]:4000",
		Candidates: []endpoint.Candidate{
			{Type: endpoint.CandidateNIC, Address: "192.0.2.10:4000", Family: "ipv4"},
			{Type: endpoint.CandidateNIC, Address: "[2001:db8::10]:4000", Family: "ipv6"},
		},
	})
	for range 2 {
		if err := mappings.apply(receivePortMappingTestValue(t, mappings.events)); err != nil {
			t.Fatal(err)
		}
	}
	requirePortMappingLane(t, mappings.lanes[0], "198.51.100.10:5000", "")
	requirePortMappingLane(t, mappings.lanes[1], "[2001:db8::10]:6000", "")

	failure := errors.New("fake IPv4 failure")
	failIPv4 <- failure
	if err := mappings.apply(receivePortMappingTestValue(t, mappings.events)); !errors.Is(err, failure) {
		t.Fatalf("port mapping event error = %v, want %v", err, failure)
	}
	requirePortMappingLane(t, mappings.lanes[0], "", failure.Error())
	requirePortMappingLane(t, mappings.lanes[1], "[2001:db8::10]:6000", "")
	if got := mappings.errorText(); got != "ipv4: "+failure.Error() {
		t.Fatalf("port mapping error = %q, want independent IPv4 error", got)
	}

	if err := mappings.stop(); err != nil {
		t.Fatal(err)
	}
	requirePortMappingLane(t, mappings.lanes[0], "", failure.Error())
	requirePortMappingLane(t, mappings.lanes[1], "", "")
}

func TestWithPortMappingsProjectsBothFamilies(t *testing.T) {
	snapshot := endpoint.Snapshot{Candidates: []endpoint.Candidate{{
		Type: endpoint.CandidateNIC, Address: "192.0.2.10:4000", Family: "ipv4",
	}}}
	ipv4 := &portmap.Mapping{
		ExternalAddress: netip.MustParseAddrPort("198.51.100.10:5000"),
		Provider:        "pcp-v4",
	}
	ipv6 := &portmap.Mapping{
		ExternalAddress: netip.MustParseAddrPort("[2001:db8::10]:6000"),
		Provider:        "pcp-v6",
	}

	projected := withPortMappings(snapshot, ipv4, ipv6)
	want := []endpoint.Candidate{
		{Type: endpoint.CandidateNIC, Address: "192.0.2.10:4000", Family: "ipv4"},
		{Type: endpoint.CandidatePortMapped, Address: "198.51.100.10:5000", Family: "ipv4", Source: "pcp-v4"},
		{Type: endpoint.CandidatePortMapped, Address: "[2001:db8::10]:6000", Family: "ipv6", Source: "pcp-v6"},
	}
	if !slices.Equal(projected.Candidates, want) {
		t.Fatalf("candidates = %v, want both port-mapping families %v", projected.Candidates, want)
	}
}

func TestPortMappingInternalPortAllowsIPv6OnlyEndpoint(t *testing.T) {
	snapshot := endpoint.Snapshot{
		ListenAddress: "[::]:4000",
		Candidates: []endpoint.Candidate{{
			Type: endpoint.CandidateNIC, Address: "[2001:db8::10]:4000", Family: "ipv6",
		}},
	}

	if port := portMappingInternalPort(snapshot, "ipv4"); port != 0 {
		t.Fatalf("IPv4 port = %d, want unavailable", port)
	}
	if port := portMappingInternalPort(snapshot, "ipv6"); port != 4000 {
		t.Fatalf("IPv6 port = %d, want 4000", port)
	}
}

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

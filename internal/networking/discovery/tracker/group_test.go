package tracker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"bork/internal/networking/discovery"
)

func TestGroupAnnouncesBoundedActualEndpointCandidates(t *testing.T) {
	type requestRecord struct {
		peerID string
		ip     string
		port   string
		event  string
	}
	requests := make(chan requestRecord, 128)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		requests <- requestRecord{peerID: query.Get("peer_id"), ip: query.Get("ip"), port: query.Get("port"), event: query.Get("event")}
		_, _ = writer.Write(httpBencodeResponse(3600, nil, nil))
	}))
	defer server.Close()
	group, err := New([]string{server.URL + "/announce"}, [20]byte{7}, testTrackerIdentity, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	candidates := []AnnounceCandidate{
		{Address: netip.MustParseAddr("8.8.8.8"), Port: 41001},
		{Address: netip.MustParseAddr("1.1.1.1"), Port: 41002},
		{Address: netip.MustParseAddr("9.9.9.9"), Port: 41003},
	}
	group.UpdateCandidates(candidates)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- group.Run(ctx, make(chan discovery.Hint, 16)) }()
	started := make(map[string]requestRecord)
	deadline := time.NewTimer(2 * time.Second)
	for len(started) < len(candidates) {
		select {
		case request := <-requests:
			if request.event == "started" {
				started[request.port] = request
			}
		case <-deadline.C:
			t.Fatalf("started registrations = %+v", started)
		}
	}
	if started["41001"].ip != "8.8.8.8" || started["41002"].ip != "1.1.1.1" || started["41003"].ip != "9.9.9.9" {
		t.Fatalf("registration candidates = %+v", started)
	}
	peerIDs := make(map[string]struct{})
	for _, request := range started {
		if len(request.peerID) != 20 {
			t.Fatalf("peer ID length = %d", len(request.peerID))
		}
		peerIDs[request.peerID] = struct{}{}
	}
	if len(peerIDs) != len(candidates) {
		t.Fatalf("registration peer IDs were not distinct: %+v", started)
	}
	cancel()
	stopped := make(map[string]requestRecord)
	for len(stopped) < len(candidates) {
		select {
		case request := <-requests:
			if request.event == "stopped" {
				stopped[request.port] = request
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("stopped registrations = %+v", stopped)
		}
	}
	for port, request := range stopped {
		if request.peerID != started[port].peerID {
			t.Fatalf("stopped registration %s used a different peer ID", port)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestGroupSnapshotKeepsStatusPerCandidate(t *testing.T) {
	group, err := New([]string{"https://tracker.example/announce"}, [20]byte{1}, testTrackerIdentity, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	first := AnnounceCandidate{Address: netip.MustParseAddr("8.8.8.8"), Port: 41001}
	second := AnnounceCandidate{Address: netip.MustParseAddr("1.1.1.1"), Port: 41002}
	group.UpdateCandidates([]AnnounceCandidate{first, second})
	firstChild, _ := newAnnouncerFromProviders(group.providers, group.infoHash, group.identityKey, first, nil, group.logger)
	secondChild, _ := newAnnouncerFromProviders(group.providers, group.infoHash, group.identityKey, second, nil, group.logger)
	firstChild.recordStatus(group.providers[0], ProviderStatus{PeerCount: 3})
	secondChild.recordStatus(group.providers[0], ProviderStatus{Error: "second failed"})
	group.setChildren(map[AnnounceCandidate]*Announcer{first: firstChild, second: secondChild})

	statuses := group.Snapshot()
	if len(statuses) != 2 || statuses[0].Candidate != first.String() || statuses[0].PeerCount != 3 || statuses[0].Error != "" ||
		statuses[1].Candidate != second.String() || statuses[1].PeerCount != 0 || statuses[1].Error != "second failed" {
		t.Fatalf("candidate statuses = %#v", statuses)
	}
}

func TestGroupCandidateNormalizationIsBoundedAndStable(t *testing.T) {
	input := []AnnounceCandidate{
		{Address: netip.MustParseAddr("8.8.8.8"), Port: 1},
		{Address: netip.MustParseAddr("8.8.8.8"), Port: 1},
		{Address: netip.MustParseAddr("1.1.1.1"), Port: 2},
		{Port: 3},
		{Address: netip.MustParseAddr("9.9.9.9"), Port: 4},
		{Address: netip.MustParseAddr("4.4.4.4"), Port: 5},
	}
	normalized := normalizeAnnounceCandidates(input)
	if len(normalized) != MaxAnnounceCandidates || normalized[0] != input[0] || normalized[1] != input[2] || normalized[2] != input[4] || normalized[3] != input[5] {
		t.Fatalf("normalized candidates = %+v", normalized)
	}
	if fallback := normalizeAnnounceCandidates([]AnnounceCandidate{{Port: 41000}}); len(fallback) != 1 || fallback[0].String() != "0.0.0.0:41000" {
		t.Fatalf("fallback candidates = %+v", fallback)
	}
}

func TestGroupCandidateChangeStopsOldRegistrationBeforeStartingNew(t *testing.T) {
	type record struct{ peerID, port, ip, event string }
	requests := make(chan record, 32)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		requests <- record{peerID: query.Get("peer_id"), port: query.Get("port"), ip: query.Get("ip"), event: query.Get("event")}
		_, _ = writer.Write(httpBencodeResponse(3600, nil, nil))
	}))
	defer server.Close()
	group, err := New([]string{server.URL + "/announce"}, [20]byte{8}, testTrackerIdentity, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	group.UpdateCandidates([]AnnounceCandidate{{Address: netip.MustParseAddr("1.1.1.1"), Port: 41001}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- group.Run(ctx, make(chan discovery.Hint, 8)) }()
	initial := waitForGroupRequest(t, requests, func(request record) bool { return request.port == "41001" && request.event == "started" })
	group.UpdateCandidates([]AnnounceCandidate{{Address: netip.MustParseAddr("8.8.8.8"), Port: 42002}})
	stopped := waitForGroupRequest(t, requests, func(request record) bool { return request.port == "41001" && request.event == "stopped" })
	updated := waitForGroupRequest(t, requests, func(request record) bool { return request.port == "42002" && request.event == "started" })
	if stopped.peerID != initial.peerID || updated.peerID == initial.peerID || updated.ip != "8.8.8.8" {
		t.Fatalf("registration transition = initial %+v stopped %+v updated %+v", initial, stopped, updated)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func waitForGroupRequest[T any](t *testing.T, requests <-chan T, match func(T) bool) T {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case request := <-requests:
			if match(request) {
				return request
			}
		case <-timer.C:
			var zero T
			t.Fatal("timed out waiting for tracker group request")
			return zero
		}
	}
}

func TestRegistrationIdentitySeparatesRoomProviderIdentityAndCandidate(t *testing.T) {
	providerA, _ := parseProvider("https://tracker-a.example/announce")
	providerB, _ := parseProvider("https://tracker-b.example/announce")
	providerWithPasskey, _ := parseProvider("https://tracker-a.example/announce?passkey=secret-a")
	providerWithOtherPasskey, _ := parseProvider("https://tracker-a.example/announce?passkey=secret-b")
	udpA, _ := parseProvider("udp://tracker-a.example:6969/announce")
	udpB, _ := parseProvider("udp://tracker-b.example:6969/announce")
	candidate := AnnounceCandidate{Address: netip.MustParseAddr("8.8.8.8"), Port: 41000}
	base, _ := newAnnouncer([]string{providerA.announceURL.String()}, [20]byte{1}, [32]byte{1}, candidate, nil, testLogger())
	same, _ := newAnnouncer([]string{providerA.announceURL.String()}, [20]byte{1}, [32]byte{1}, candidate, nil, testLogger())
	differentRoom, _ := newAnnouncer([]string{providerA.announceURL.String()}, [20]byte{2}, [32]byte{1}, candidate, nil, testLogger())
	differentIdentity, _ := newAnnouncer([]string{providerA.announceURL.String()}, [20]byte{1}, [32]byte{2}, candidate, nil, testLogger())
	registrations := []trackerRegistration{
		base.registration(providerA, candidate),
		same.registration(providerA, candidate),
		differentRoom.registration(providerA, candidate),
		differentIdentity.registration(providerA, candidate),
		base.registration(providerB, candidate),
		base.registration(providerWithPasskey, candidate),
		base.registration(providerWithOtherPasskey, candidate),
		base.registration(udpA, candidate),
		base.registration(udpB, candidate),
		base.registration(providerA, AnnounceCandidate{Address: candidate.Address, Port: candidate.Port + 1}),
	}
	if registrations[0] != registrations[1] {
		t.Fatal("same registration identity was not deterministic")
	}
	seen := make(map[[20]byte]struct{})
	for index, registration := range registrations {
		if index == 1 {
			continue
		}
		if _, exists := seen[registration.peerID]; exists {
			t.Fatalf("registration %d reused a peer ID", index)
		}
		seen[registration.peerID] = struct{}{}
	}
	if udpA.scope != "udp://tracker-a.example:6969/announce" || udpB.scope != "udp://tracker-b.example:6969/announce" || udpA.scope == udpB.scope {
		t.Fatalf("UDP provider scopes were not distinct: %q, %q", udpA.scope, udpB.scope)
	}
	if providerWithPasskey.display != providerWithOtherPasskey.display || strings.Contains(providerWithPasskey.display, "?") {
		t.Fatalf("private tracker query leaked into display: %q, %q", providerWithPasskey.display, providerWithOtherPasskey.display)
	}
	if providerWithPasskey.scope == providerWithOtherPasskey.scope {
		t.Fatal("private tracker queries reused a provider scope")
	}
}

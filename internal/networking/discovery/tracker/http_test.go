package tracker

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bork/internal/networking/discovery"
)

func TestHTTPProviderURLValidation(t *testing.T) {
	valid := []string{
		"http://tracker.example/announce",
		"https://tracker.example:8443/announce",
		"https://[2001:db8::1]/announce?passkey=abc&tag=one&tag=two",
	}
	for _, raw := range valid {
		configured, err := parseProvider(raw)
		if err != nil {
			t.Errorf("parseProvider(%q) error = %v", raw, err)
			continue
		}
		if configured.scheme != "http" && configured.scheme != "https" {
			t.Errorf("parseProvider(%q) scheme = %q", raw, configured.scheme)
		}
		if configured.announceURL.RawQuery != "" && strings.Contains(configured.display, "?") {
			t.Errorf("parseProvider(%q) display leaks configured query: %q", raw, configured.display)
		}
	}
	invalid := []string{
		"http://",
		"https://user:pass@tracker.example/announce",
		"https://tracker.example/announce#",
		"https://tracker.example:0/announce",
		"https://tracker.example/announce?%69nfo_hash=duplicate",
		"https://tracker.example/announce?bad=%zz",
		strings.Repeat("x", maxProviderURLLength+1),
	}
	for key := range generatedHTTPQueryKeys {
		invalid = append(invalid, "https://tracker.example/announce?"+url.QueryEscape(key)+"=duplicate")
	}
	tooMany := make(url.Values)
	for index := range maxConfiguredQueryKeys + 1 {
		tooMany.Set("key"+strconv.Itoa(index), "value")
	}
	invalid = append(invalid, "https://tracker.example/announce?"+tooMany.Encode())
	for _, raw := range invalid {
		if _, err := parseProvider(raw); err == nil {
			t.Errorf("parseProvider(%q) error = nil", raw)
		}
	}
}

func TestHTTPTrackerLifecycle(t *testing.T) {
	peer4 := netip.MustParseAddrPort("198.51.100.70:5000")
	peer6 := netip.MustParseAddrPort("[2001:db8::70]:5001")
	body := httpBencodeResponse(0, []netip.AddrPort{
		peer4,
		netip.MustParseAddrPort("239.1.1.1:5002"),
	}, []netip.AddrPort{peer6})
	requests := make(chan recordedHTTPRequest, 128)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		record := recordedHTTPRequest{
			at:     time.Now(),
			method: request.Method,
			path:   request.URL.Path,
			query:  cloneQuery(request.URL.Query()),
		}
		select {
		case requests <- record:
		case <-request.Context().Done():
			return
		}
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = writer.Write(body)
	}))
	defer server.Close()

	var infoHash [20]byte
	for index := range infoHash {
		infoHash[index] = byte(0xf0 + index)
	}
	announcer, err := newAnnouncer([]string{server.URL + "/announce?passkey=abc123"}, infoHash, testTrackerIdentity, AnnounceCandidate{Port: 41001}, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	announcer.timing = testAnnouncerTiming()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	hints := make(chan discovery.Hint, 128)
	go func() { done <- announcer.Run(ctx, hints) }()

	first := waitForHTTPRequest(t, requests, func(record recordedHTTPRequest) bool {
		return record.query.Get("event") == "started"
	})
	if first.method != http.MethodGet || first.path != "/announce" {
		t.Fatalf("request = %s %s", first.method, first.path)
	}
	assertSingleQueryValue(t, first.query, "info_hash", string(infoHash[:]))
	registration := announcer.registration(announcer.providers[0], AnnounceCandidate{Port: 41001})
	assertSingleQueryValue(t, first.query, "peer_id", string(registration.peerID[:]))
	assertSingleQueryValue(t, first.query, "port", "41001")
	assertSingleQueryValue(t, first.query, "uploaded", "0")
	assertSingleQueryValue(t, first.query, "downloaded", "0")
	assertSingleQueryValue(t, first.query, "left", "0")
	assertSingleQueryValue(t, first.query, "compact", "1")
	assertSingleQueryValue(t, first.query, "numwant", strconv.Itoa(maxAnnouncePeers))
	assertSingleQueryValue(t, first.query, "event", "started")
	assertSingleQueryValue(t, first.query, "passkey", "abc123")

	select {
	case hint := <-hints:
		if hint.Address != peer4 || hint.Source != discovery.SourceTracker {
			t.Fatalf("hint = %#v", hint)
		}
		lifetime := hint.ExpiresAt.Sub(first.at)
		if lifetime < 45*time.Millisecond || lifetime > 100*time.Millisecond {
			t.Fatalf("hint lifetime = %v", lifetime)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for HTTP tracker hint")
	}
	status := announcer.Snapshot()
	if len(status) != 1 || status[0].PeerCount != 2 || status[0].Error != "" {
		t.Fatalf("tracker status = %+v", status)
	}

	periodic := waitForHTTPRequest(t, requests, func(record recordedHTTPRequest) bool {
		_, hasEvent := record.query["event"]
		return !hasEvent
	})
	assertSingleQueryValue(t, periodic.query, "port", "41001")

	cancel()
	stopped := waitForHTTPRequest(t, requests, func(record recordedHTTPRequest) bool {
		return record.query.Get("port") == "41001" && record.query.Get("event") == "stopped"
	})
	assertSingleQueryValue(t, stopped.query, "peer_id", string(registration.peerID[:]))
	if err := waitForRun(t, done); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestDefaultAnnounceCadenceSupportsMutualNATPunching(t *testing.T) {
	timing := normalizeTiming(defaultAnnouncerTiming)
	providerInterval := 3 * time.Hour
	for announcement := range timing.initialAnnounces {
		if interval := effectiveAnnounceInterval(providerInterval, timing, announcement); interval != 5*time.Second {
			t.Fatalf("initial announce %d interval = %s", announcement, interval)
		}
	}
	if interval := effectiveAnnounceInterval(providerInterval, timing, timing.initialAnnounces); interval != 30*time.Second {
		t.Fatalf("steady announce interval = %s", interval)
	}
}

func TestHTTPTrackerRediscoversPeerJoiningAfterFirstAnnounce(t *testing.T) {
	var mu sync.Mutex
	peersByID := make(map[string]netip.AddrPort)
	requests := make(chan struct{}, 16)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		peerID := request.URL.Query().Get("peer_id")
		port, _ := strconv.ParseUint(request.URL.Query().Get("port"), 10, 16)
		mu.Lock()
		if _, exists := peersByID[peerID]; !exists {
			index := byte(len(peersByID) + 1)
			peersByID[peerID] = netip.AddrPortFrom(netip.AddrFrom4([4]byte{198, 51, 100, index}), uint16(port))
		}
		peers := make([]netip.AddrPort, 0, len(peersByID))
		for _, peer := range peersByID {
			peers = append(peers, peer)
		}
		mu.Unlock()
		select {
		case requests <- struct{}{}:
		default:
		}
		_, _ = writer.Write(httpBencodeResponse(3600, peers, nil))
	}))
	defer server.Close()

	newTestAnnouncer := func(port uint16) (*Announcer, chan discovery.Hint, context.CancelFunc, <-chan error) {
		announcer, err := newAnnouncer([]string{server.URL + "/announce"}, [20]byte{9}, testTrackerIdentity, AnnounceCandidate{Port: port}, nil, testLogger())
		if err != nil {
			t.Fatal(err)
		}
		announcer.timing = testAnnouncerTiming()
		announcer.timing.intervalMin = 20 * time.Millisecond
		announcer.timing.intervalMax = time.Hour
		announcer.timing.initialInterval = 20 * time.Millisecond
		announcer.timing.initialAnnounces = 3
		ctx, cancel := context.WithCancel(context.Background())
		hints := make(chan discovery.Hint, 32)
		done := make(chan error, 1)
		go func() { done <- announcer.Run(ctx, hints) }()
		return announcer, hints, cancel, done
	}

	_, firstHints, stopFirst, firstDone := newTestAnnouncer(41001)
	t.Cleanup(stopFirst)
	select {
	case <-requests:
	case <-time.After(time.Second):
		t.Fatal("first peer did not announce")
	}
	_, _, stopSecond, secondDone := newTestAnnouncer(41002)
	t.Cleanup(stopSecond)
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	foundSecond := false
	for !foundSecond {
		select {
		case hint := <-firstHints:
			foundSecond = hint.Address.Port() == 41002
		case <-deadline.C:
			t.Fatal("first peer did not rediscover the later peer")
		}
	}
	stopFirst()
	stopSecond()
	if err := waitForRun(t, firstDone); err != nil {
		t.Fatal(err)
	}
	if err := waitForRun(t, secondDone); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPProviderFailureUpdatesStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	announcer, err := newAnnouncer([]string{server.URL + "/announce"}, [20]byte{1}, testTrackerIdentity, AnnounceCandidate{Port: 41000}, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	announcer.timing = testAnnouncerTiming()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- announcer.Run(ctx, make(chan discovery.Hint, 1)) }()
	select {
	case <-announcer.StatusChanges():
		status := announcer.Snapshot()
		if len(status) != 1 || !strings.Contains(status[0].Error, "503") || status[0].NextAnnounce == "" {
			t.Fatalf("failure status = %+v", status)
		}
	case <-time.After(time.Second):
		t.Fatal("tracker failure did not update status")
	}
	cancel()
	if err := waitForRun(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPTrackerCancellationStopsInflightRequest(t *testing.T) {
	entered := make(chan struct{})
	canceled := make(chan struct{})
	var enteredOnce, canceledOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		enteredOnce.Do(func() { close(entered) })
		<-request.Context().Done()
		canceledOnce.Do(func() { close(canceled) })
	}))
	defer server.Close()
	announcer, err := newAnnouncer([]string{server.URL + "/announce"}, [20]byte{2}, testTrackerIdentity, AnnounceCandidate{Port: 41000}, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	announcer.timing = testAnnouncerTiming()
	announcer.timing.httpRequestTimeout = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- announcer.Run(ctx, make(chan discovery.Hint)) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("HTTP tracker request did not start")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("HTTP tracker request context was not canceled")
	}
	if err := waitForRun(t, done); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestHTTPTrackerValidatesStatusAndBodyBounds(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   []byte
	}{
		{name: "status", status: http.StatusServiceUnavailable, body: httpBencodeResponse(30, nil, nil)},
		{name: "oversized body", status: http.StatusOK, body: bytes.Repeat([]byte{'x'}, maxHTTPTrackerResponseSize+1)},
		{name: "malformed body", status: http.StatusOK, body: []byte("not bencode")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.status)
				_, _ = writer.Write(test.body)
			}))
			defer server.Close()
			announcer, err := newAnnouncer([]string{server.URL + "/announce"}, [20]byte{}, testTrackerIdentity, AnnounceCandidate{Port: 5000}, nil, testLogger())
			if err != nil {
				t.Fatal(err)
			}
			timing := normalizeTiming(testAnnouncerTiming())
			registration := announcer.registration(announcer.providers[0], AnnounceCandidate{Port: 5000})
			if _, err := announcer.httpAnnounce(context.Background(), announcer.providers[0], registration, true, timing); err == nil {
				t.Fatal("httpAnnounce() error = nil")
			}
		})
	}
}

func TestHTTPTrackerSafeRedirectPreservesQuery(t *testing.T) {
	redirected := make(chan url.Values, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/announce" {
			http.Redirect(writer, request, "/moved", http.StatusFound)
			return
		}
		redirected <- cloneQuery(request.URL.Query())
		_, _ = writer.Write(httpBencodeResponse(30, nil, nil))
	}))
	defer server.Close()
	announcer, err := newAnnouncer([]string{server.URL + "/announce?passkey=abc"}, [20]byte{3}, testTrackerIdentity, AnnounceCandidate{Port: 5000}, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	timing := normalizeTiming(testAnnouncerTiming())
	registration := announcer.registration(announcer.providers[0], AnnounceCandidate{Port: 5000})
	if _, err := announcer.httpAnnounce(context.Background(), announcer.providers[0], registration, true, timing); err != nil {
		t.Fatalf("httpAnnounce() error = %v", err)
	}
	select {
	case query := <-redirected:
		assertSingleQueryValue(t, query, "passkey", "abc")
		assertSingleQueryValue(t, query, "port", "5000")
		assertSingleQueryValue(t, query, "event", "started")
	case <-time.After(time.Second):
		t.Fatal("redirect target was not requested")
	}
}

func TestHTTPAndUDPProviderFailuresAreIsolated(t *testing.T) {
	var httpCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		httpCalls.Add(1)
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	udpAddress := netip.MustParseAddrPort("192.0.2.80:6969")
	peer := netip.MustParseAddrPort("198.51.100.80:5000")
	transport := newFakeTrackerTransport()
	transport.peers[udpAddress] = []netip.AddrPort{peer}
	announcer, err := newAnnouncer([]string{
		server.URL + "/announce",
		"udp://192.0.2.80:6969/announce",
	}, [20]byte{4}, testTrackerIdentity, AnnounceCandidate{Port: 5000}, transport, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	announcer.timing = testAnnouncerTiming()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	hints := make(chan discovery.Hint, 8)
	go func() { done <- announcer.Run(ctx, hints) }()
	select {
	case hint := <-hints:
		if hint.Address != peer {
			t.Fatalf("hint = %#v", hint)
		}
	case <-time.After(time.Second):
		t.Fatal("failed HTTP provider blocked UDP provider")
	}
	waitForCondition(t, time.Second, func() bool {
		return httpCalls.Load() > 0 && transport.callCount(udpAddress) > 0
	}, "HTTP and UDP provider attempts")
	cancel()
	if err := waitForRun(t, done); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

type recordedHTTPRequest struct {
	at     time.Time
	method string
	path   string
	query  url.Values
}

func cloneQuery(query url.Values) url.Values {
	cloned := make(url.Values, len(query))
	for key, values := range query {
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}

func assertSingleQueryValue(t *testing.T, query url.Values, key, want string) {
	t.Helper()
	values := query[key]
	if len(values) != 1 || values[0] != want {
		t.Fatalf("query[%q] = %q, want one value %q", key, values, want)
	}
}

func waitForHTTPRequest(t *testing.T, requests <-chan recordedHTTPRequest, match func(recordedHTTPRequest) bool) recordedHTTPRequest {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case request := <-requests:
			if match(request) {
				return request
			}
		case <-timer.C:
			t.Fatal("timed out waiting for HTTP announce")
		}
	}
}

func TestHTTPTrackerRedirectPolicyRejectsUnsafeTargets(t *testing.T) {
	original, _ := http.NewRequest(http.MethodGet, "https://tracker.example/announce?port=1", nil)
	tests := []string{
		"http://tracker.example/announce?port=1",
		"https://other.example/announce?port=1",
		"ftp://tracker.example/announce?port=1",
	}
	for _, target := range tests {
		request, _ := http.NewRequest(http.MethodGet, target, nil)
		if err := trackerRedirectPolicy(request, []*http.Request{original}); err == nil {
			t.Errorf("redirect to %s was accepted", target)
		}
	}
	request, _ := http.NewRequest(http.MethodGet, "https://tracker.example/announce?port=1", nil)
	via := []*http.Request{original, original, original, original}
	if err := trackerRedirectPolicy(request, via); err == nil {
		t.Fatal("redirect limit was not enforced")
	}
}

func TestHTTPClientBoundsResponseHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Oversized", strings.Repeat("x", int(maxHTTPResponseHeaders)+1))
		_, _ = writer.Write(httpBencodeResponse(30, nil, nil))
	}))
	defer server.Close()

	client := newHTTPClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("tracker transport type = %T", client.Transport)
	}
	if transport == http.DefaultTransport || http.DefaultTransport.(*http.Transport).MaxResponseHeaderBytes != 0 {
		t.Fatal("default HTTP transport was mutated")
	}
	if client.Timeout != defaultAnnouncerTiming.httpRequestTimeout || client.CheckRedirect == nil ||
		transport.MaxResponseHeaderBytes != maxHTTPResponseHeaders || transport.ResponseHeaderTimeout != defaultAnnouncerTiming.httpRequestTimeout {
		t.Fatalf("HTTP transport bounds = headers %d, timeout %s", transport.MaxResponseHeaderBytes, transport.ResponseHeaderTimeout)
	}
	response, err := client.Get(server.URL)
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatal("HTTP client accepted oversized response headers")
	}
}

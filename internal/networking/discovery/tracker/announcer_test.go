package tracker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"bork/internal/networking/discovery"
)

var testTrackerIdentity = [32]byte{1}

func TestNewAnnouncerValidatesUDPProvidersAndConfiguration(t *testing.T) {
	transport := transportFunc(func(context.Context, netip.AddrPort, []byte, uint32, uint32) ([]byte, error) {
		return nil, errors.New("unused")
	})
	candidate := AnnounceCandidate{Port: 41000}
	valid := []string{
		"udp://tracker.example:6969/announce",
		"udp://192.0.2.1:80",
		"udp://[2001:db8::1]:1337/announce",
	}
	for _, raw := range valid {
		if _, err := newAnnouncer([]string{raw}, [20]byte{}, testTrackerIdentity, candidate, transport, testLogger()); err != nil {
			t.Errorf("newAnnouncer(%q) error = %v", raw, err)
		}
	}
	invalid := []string{
		"",
		" udp://tracker.example:80",
		"ftp://tracker.example/announce",
		"udp://tracker.example/announce",
		"udp://tracker.example:0/announce",
		"udp://user@tracker.example:80/announce",
		"udp://tracker.example:80/announce?key=value",
		"udp://tracker.example:80/announce#fragment",
	}
	for _, raw := range invalid {
		if _, err := newAnnouncer([]string{raw}, [20]byte{}, testTrackerIdentity, candidate, transport, testLogger()); err == nil {
			t.Errorf("newAnnouncer(%q) error = nil", raw)
		}
	}
	if _, err := newAnnouncer([]string{"https://tracker.example/announce"}, [20]byte{9}, [32]byte{}, candidate, nil, testLogger()); err == nil {
		t.Fatal("newAnnouncer() accepted an empty identity key")
	}
	if _, err := newAnnouncer(nil, [20]byte{}, [32]byte{}, AnnounceCandidate{}, nil, testLogger()); err != nil {
		t.Fatalf("newAnnouncer() rejected disabled trackers with nil transport: %v", err)
	}
	if _, err := newAnnouncer([]string{"https://tracker.example/announce"}, [20]byte{}, testTrackerIdentity, candidate, nil, testLogger()); err != nil {
		t.Fatalf("newAnnouncer() rejected HTTP-only trackers with nil transport: %v", err)
	}
	if _, err := newAnnouncer([]string{"udp://tracker.example:80/announce"}, [20]byte{}, testTrackerIdentity, candidate, nil, testLogger()); err == nil {
		t.Fatal("newAnnouncer() accepted a UDP tracker with nil transport")
	}
}

func TestObservedRegistrationAddressRequiresTrackerEvidence(t *testing.T) {
	candidate := AnnounceCandidate{Address: netip.MustParseAddr("8.8.8.8"), Port: 41000}
	if got := observedRegistrationAddress(announceResponse{externalAddress: netip.MustParseAddr("1.1.1.1")}, candidate); got != "1.1.1.1:41000" {
		t.Fatalf("external-ip observation = %q", got)
	}
	if got := observedRegistrationAddress(announceResponse{peers: []netip.AddrPort{netip.MustParseAddrPort("8.8.8.8:41000")}}, candidate); got != "8.8.8.8:41000" {
		t.Fatalf("exact observation = %q", got)
	}
	observed := AnnounceCandidate{Port: 41000}
	if got := observedRegistrationAddress(announceResponse{peers: []netip.AddrPort{netip.MustParseAddrPort("9.9.9.9:41000")}}, observed); got != "9.9.9.9:41000" {
		t.Fatalf("unique-port observation = %q", got)
	}
	ambiguous := announceResponse{peers: []netip.AddrPort{
		netip.MustParseAddrPort("9.9.9.9:41000"), netip.MustParseAddrPort("1.1.1.1:41000"),
	}}
	if got := observedRegistrationAddress(ambiguous, observed); got != "" {
		t.Fatalf("ambiguous observation = %q", got)
	}
}

func TestResolveProviderBoundsBothAddressFamilies(t *testing.T) {
	announcer, err := newAnnouncer([]string{"udp://tracker.example:6969/announce"}, [20]byte{}, testTrackerIdentity, AnnounceCandidate{Port: 41000}, &fakeTrackerTransport{}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	var lookups atomic.Int32
	announcer.lookupNetIP = func(_ context.Context, network, host string) ([]netip.Addr, error) {
		lookups.Add(1)
		if network != "ip" || host != "tracker.example" {
			return nil, fmt.Errorf("lookup = %q, %q", network, host)
		}
		addresses := []netip.Addr{netip.IPv4Unspecified(), netip.MustParseAddr("ff02::1")}
		for index := 1; index <= 7; index++ {
			addresses = append(addresses, netip.AddrFrom4([4]byte{192, 0, 2, byte(index)}))
		}
		for index := 1; index <= 7; index++ {
			addresses = append(addresses, netip.MustParseAddr(fmt.Sprintf("2001:db8::%x", index)))
		}
		addresses = append(addresses, netip.MustParseAddr("192.0.2.1"))
		return addresses, nil
	}
	resolved, err := announcer.resolveProvider(context.Background(), announcer.providers[0], time.Second)
	if err != nil {
		t.Fatalf("resolveProvider() error = %v", err)
	}
	if len(resolved) != maxResolvedAddresses {
		t.Fatalf("resolved address count = %d, want %d", len(resolved), maxResolvedAddresses)
	}
	ipv4, ipv6 := 0, 0
	for _, address := range resolved {
		if address.Port() != 6969 || !usableTrackerAddress(address.Addr()) {
			t.Fatalf("resolved unusable address %s", address)
		}
		if address.Addr().Is4() {
			ipv4++
		} else {
			ipv6++
		}
	}
	if ipv4 != maxResolvedAddressesByIP || ipv6 != maxResolvedAddressesByIP {
		t.Fatalf("resolved families = IPv4 %d, IPv6 %d", ipv4, ipv6)
	}

	literal, err := parseProvider("udp://192.0.2.20:7000/announce")
	if err != nil {
		t.Fatal(err)
	}
	got, err := announcer.resolveProvider(context.Background(), literal, time.Second)
	if err != nil || len(got) != 1 || got[0] != netip.MustParseAddrPort("192.0.2.20:7000") {
		t.Fatalf("literal resolve = %v, %v", got, err)
	}
	if lookups.Load() != 1 {
		t.Fatalf("DNS lookup count = %d, want 1", lookups.Load())
	}
}

func TestAnnouncerLifecycle(t *testing.T) {
	trackerAddress := netip.MustParseAddrPort("192.0.2.10:6969")
	peerAddress := netip.MustParseAddrPort("198.51.100.20:41000")
	transport := newFakeTrackerTransport()
	transport.peers[trackerAddress] = []netip.AddrPort{
		peerAddress,
		netip.MustParseAddrPort("239.1.1.1:5000"),
		netip.MustParseAddrPort("198.51.100.21:0"),
	}
	var infoHash [20]byte
	copy(infoHash[:], "private swarm hash!!")
	announcer, err := newAnnouncer([]string{"udp://192.0.2.10:6969/announce"}, infoHash, testTrackerIdentity, AnnounceCandidate{Port: 41001}, transport, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	announcer.timing = testAnnouncerTiming()

	ctx, cancel := context.WithCancel(context.Background())
	hints := make(chan discovery.Hint, 64)
	done := make(chan error, 1)
	go func() { done <- announcer.Run(ctx, hints) }()

	first := waitForAnnounce(t, transport.announced, func(record recordedAnnounce) bool {
		return record.destination == trackerAddress
	})
	if first.event != eventStarted {
		t.Fatalf("initial event = %d, want started", first.event)
	}
	firstRegistration := announcer.registration(announcer.providers[0], AnnounceCandidate{Port: 41001})
	if first.port != 41001 || first.infoHash != infoHash || first.peerID != firstRegistration.peerID || first.key != firstRegistration.key {
		t.Fatalf("initial announce = %#v", first)
	}
	if first.transaction == 0 || first.connectionID == 0 || first.numWant < 0 || first.numWant > maxAnnouncePeers {
		t.Fatalf("initial announce identifiers/bounds = %#v", first)
	}
	if first.downloaded != 0 || first.left != 0 || first.uploaded != 0 || first.ip != 0 {
		t.Fatalf("initial announce counters/IP are nonzero: %#v", first)
	}

	select {
	case hint := <-hints:
		if hint.Address != peerAddress || hint.Source != discovery.SourceTracker {
			t.Fatalf("hint = %#v", hint)
		}
		lifetime := hint.ExpiresAt.Sub(first.recordedAt)
		if lifetime < 45*time.Millisecond || lifetime > 100*time.Millisecond {
			t.Fatalf("hint lifetime = %v", lifetime)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tracker hint")
	}

	periodic := waitForAnnounce(t, transport.announced, func(record recordedAnnounce) bool {
		return record.destination == trackerAddress && record.event == eventNone
	})
	if periodic.port != 41001 {
		t.Fatalf("periodic port = %d", periodic.port)
	}

	waitForCondition(t, time.Second, func() bool { return transport.connectCount(trackerAddress) >= 2 }, "connection ID renewal")

	cancel()
	if err := waitForRun(t, done); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	stopped := false
	for _, record := range transport.snapshotAnnounces() {
		if record.transaction == 0 {
			t.Fatal("announce used a zero transaction")
		}
		expected := announcer.registration(announcer.providers[0], AnnounceCandidate{Port: 41001})
		if record.peerID != expected.peerID || record.key != expected.key {
			t.Fatal("tracker identity did not match its registration candidate")
		}
		stopped = stopped || record.event == eventStopped
	}
	if !stopped {
		t.Fatal("tracker registration was not stopped")
	}
}

func TestProviderRetryBackoffResetsAfterAcceptedAnnounce(t *testing.T) {
	trackerAddress := netip.MustParseAddrPort("192.0.2.30:6969")
	peerAddress := netip.MustParseAddrPort("198.51.100.30:41000")
	transport := newFakeTrackerTransport()
	transport.failures[trackerAddress] = errors.New("provider failed")
	transport.peers[trackerAddress] = []netip.AddrPort{peerAddress}
	announcer, err := newAnnouncer([]string{"udp://192.0.2.30:6969/announce"}, [20]byte{3}, testTrackerIdentity, AnnounceCandidate{Port: 41001}, transport, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	announcer.timing = testAnnouncerTiming()
	announcer.timing.providerRetry = time.Second
	announcer.timing.providerRetryMax = 4 * time.Second
	retries := make(chan recordedRetryWait, 8)
	announcer.waitRetry = func(ctx context.Context, delay time.Duration) bool {
		request := recordedRetryWait{delay: delay, release: make(chan struct{})}
		select {
		case retries <- request:
		case <-ctx.Done():
			return false
		}
		select {
		case <-request.release:
			return true
		case <-ctx.Done():
			return false
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	hints := make(chan discovery.Hint, 1)
	go func() { done <- announcer.Run(ctx, hints) }()
	wantDelays := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	for _, want := range wantDelays {
		request := waitForRetryWait(t, retries)
		if request.delay != want {
			cancel()
			t.Fatalf("provider retry delay = %s, want %s", request.delay, want)
		}
		if want == wantDelays[len(wantDelays)-1] {
			transport.setFailure(trackerAddress, nil)
		}
		close(request.release)
	}
	select {
	case hint := <-hints:
		if hint.Address != peerAddress {
			cancel()
			t.Fatalf("tracker hint = %+v", hint)
		}
	case <-time.After(time.Second):
		cancel()
		t.Fatal("tracker response was not accepted and published")
	}
	transport.setFailure(trackerAddress, errors.New("provider failed again"))
	request := waitForRetryWait(t, retries)
	if request.delay != time.Second {
		cancel()
		t.Fatalf("retry after successful announce = %s, want %s", request.delay, time.Second)
	}
	cancel()
	if err := waitForRun(t, done); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestProvidersAreIsolated(t *testing.T) {
	bad := netip.MustParseAddrPort("192.0.2.40:6969")
	good := netip.MustParseAddrPort("192.0.2.41:6969")
	peer := netip.MustParseAddrPort("198.51.100.40:5000")
	transport := newFakeTrackerTransport()
	transport.failures[bad] = errors.New("provider failed")
	transport.peers[good] = []netip.AddrPort{peer}
	announcer, err := newAnnouncer([]string{
		"udp://192.0.2.40:6969/announce",
		"udp://192.0.2.41:6969/announce",
	}, [20]byte{2}, testTrackerIdentity, AnnounceCandidate{Port: 5001}, transport, testLogger())
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
			t.Fatalf("hint address = %s", hint.Address)
		}
	case <-time.After(time.Second):
		t.Fatal("healthy provider was blocked by failed provider")
	}
	waitForCondition(t, time.Second, func() bool {
		return transport.callCount(bad) > 0 && transport.callCount(good) > 0
	}, "both provider workers")
	if transport.callCount(bad) == 0 || transport.callCount(good) == 0 {
		t.Fatalf("provider calls = bad %d, good %d", transport.callCount(bad), transport.callCount(good))
	}
	cancel()
	if err := waitForRun(t, done); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestNoProvidersWaitsForCancellationWithoutIO(t *testing.T) {
	transport := newFakeTrackerTransport()
	announcer, err := newAnnouncer(nil, [20]byte{}, [32]byte{}, AnnounceCandidate{}, transport, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	var lookups atomic.Int32
	announcer.lookupNetIP = func(context.Context, string, string) ([]netip.Addr, error) {
		lookups.Add(1)
		return nil, errors.New("unexpected DNS")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- announcer.Run(ctx, make(chan discovery.Hint)) }()
	select {
	case err := <-done:
		t.Fatalf("Run() returned before cancellation: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if lookups.Load() != 0 || transport.totalCallCount() != 0 {
		t.Fatalf("disabled tracker performed I/O: DNS %d, transport %d", lookups.Load(), transport.totalCallCount())
	}
	cancel()
	if err := waitForRun(t, done); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestExchangeRetransmitsAndValidatesResponse(t *testing.T) {
	const transaction = 0x10203040
	var calls atomic.Int32
	transport := transportFunc(func(_ context.Context, _ netip.AddrPort, _ []byte, action, gotTransaction uint32) ([]byte, error) {
		if action != actionConnect || gotTransaction != transaction {
			return nil, fmt.Errorf("exchange metadata = %d/%08x", action, gotTransaction)
		}
		call := calls.Add(1)
		response := make([]byte, connectResponseSize)
		binary.BigEndian.PutUint32(response[4:8], transaction)
		binary.BigEndian.PutUint64(response[8:16], 7)
		if call == 1 {
			binary.BigEndian.PutUint32(response[0:4], actionAnnounce)
		}
		return response, nil
	})
	announcer, err := newAnnouncer(nil, [20]byte{}, [32]byte{}, AnnounceCandidate{}, transport, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	timing := testAnnouncerTiming()
	timing.requestAttempts = 2
	response, err := announcer.exchange(context.Background(), netip.MustParseAddrPort("192.0.2.50:80"), marshalConnectRequest(transaction), actionConnect, transaction, timing)
	if err != nil {
		t.Fatalf("exchange() error = %v", err)
	}
	if _, err := parseConnectResponse(response, transaction); err != nil {
		t.Fatalf("validated response error = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("exchange call count = %d, want 2", calls.Load())
	}

	calls.Store(0)
	announcer.transport = transportFunc(func(_ context.Context, _ netip.AddrPort, _ []byte, _, gotTransaction uint32) ([]byte, error) {
		calls.Add(1)
		response := make([]byte, responseHeaderSize+4)
		binary.BigEndian.PutUint32(response[0:4], actionError)
		binary.BigEndian.PutUint32(response[4:8], gotTransaction)
		copy(response[8:], "nope")
		return response, nil
	})
	_, err = announcer.exchange(context.Background(), netip.MustParseAddrPort("192.0.2.50:80"), marshalConnectRequest(transaction), actionConnect, transaction, timing)
	var trackerErr *TrackerError
	if !errors.As(err, &trackerErr) || calls.Load() != 1 {
		t.Fatalf("tracker error exchange = %v after %d calls", err, calls.Load())
	}
}

func TestRandomTransactionsAreNonzero(t *testing.T) {
	source := bytes.NewReader([]byte{
		0, 0, 0, 0,
		0, 0, 0, 7,
	})
	transaction, err := readNonzeroUint32(source)
	if err != nil || transaction != 7 {
		t.Fatalf("readNonzeroUint32() = %d, %v", transaction, err)
	}
	if _, err := readNonzeroUint32(bytes.NewReader(make([]byte, 4))); err == nil {
		t.Fatal("readNonzeroUint32() accepted an exhausted zero source")
	}
	if got := exponentialTimeout(15*time.Second, 3); got != 120*time.Second {
		t.Fatalf("exponential timeout = %v", got)
	}
	if got := normalizeTiming(announcerTiming{connectionLifetime: 2 * time.Minute}).connectionLifetime; got != maximumConnectionLifetime {
		t.Fatalf("connection lifetime = %v", got)
	}
}

func TestBoundedTrackerTextPreservesUTF8(t *testing.T) {
	text := boundedTrackerText(strings.Repeat("é", maxTrackerErrorLength), maxTrackerErrorLength)
	if len(text) > maxTrackerErrorLength || !utf8.ValidString(text) {
		t.Fatalf("bounded tracker text length/UTF-8 = %d, %q", len(text), text)
	}
	invalid := boundedTrackerText(string([]byte{'x', 0xff, 'y'}), maxTrackerErrorLength)
	if !utf8.ValidString(invalid) {
		t.Fatalf("invalid tracker text was not sanitized: %q", invalid)
	}
}

type transportFunc func(context.Context, netip.AddrPort, []byte, uint32, uint32) ([]byte, error)

func (f transportFunc) ExchangeTracker(ctx context.Context, address netip.AddrPort, request []byte, action, transaction uint32) ([]byte, error) {
	return f(ctx, address, request, action, transaction)
}

type recordedAnnounce struct {
	destination  netip.AddrPort
	recordedAt   time.Time
	connectionID uint64
	transaction  uint32
	infoHash     [20]byte
	peerID       [20]byte
	downloaded   uint64
	left         uint64
	uploaded     uint64
	event        uint32
	ip           uint32
	key          uint32
	numWant      int32
	port         uint16
}

type recordedRetryWait struct {
	delay   time.Duration
	release chan struct{}
}

type fakeTrackerTransport struct {
	mu         sync.Mutex
	failures   map[netip.AddrPort]error
	peers      map[netip.AddrPort][]netip.AddrPort
	interval   uint32
	calls      map[netip.AddrPort]int
	connects   map[netip.AddrPort]int
	announces  []recordedAnnounce
	announced  chan recordedAnnounce
	totalCalls int
	connection uint64
}

func newFakeTrackerTransport() *fakeTrackerTransport {
	return &fakeTrackerTransport{
		failures:  make(map[netip.AddrPort]error),
		peers:     make(map[netip.AddrPort][]netip.AddrPort),
		calls:     make(map[netip.AddrPort]int),
		connects:  make(map[netip.AddrPort]int),
		announced: make(chan recordedAnnounce, 256),
	}
}

func (f *fakeTrackerTransport) ExchangeTracker(ctx context.Context, destination netip.AddrPort, request []byte, expectedAction, transaction uint32) ([]byte, error) {
	f.mu.Lock()
	f.totalCalls++
	f.calls[destination]++
	failure := f.failures[destination]
	f.mu.Unlock()
	if failure != nil {
		return nil, failure
	}
	if transaction == 0 {
		return nil, errors.New("zero transaction")
	}

	switch expectedAction {
	case actionConnect:
		if len(request) != connectRequestSize || binary.BigEndian.Uint64(request[0:8]) != protocolConnectionID || binary.BigEndian.Uint32(request[8:12]) != actionConnect || binary.BigEndian.Uint32(request[12:16]) != transaction {
			return nil, errors.New("malformed connect request")
		}
		f.mu.Lock()
		f.connects[destination]++
		f.connection++
		connectionID := f.connection
		f.mu.Unlock()
		response := make([]byte, connectResponseSize)
		binary.BigEndian.PutUint32(response[0:4], actionConnect)
		binary.BigEndian.PutUint32(response[4:8], transaction)
		binary.BigEndian.PutUint64(response[8:16], connectionID)
		return response, nil
	case actionAnnounce:
		if len(request) != announceRequestSize || binary.BigEndian.Uint32(request[8:12]) != actionAnnounce || binary.BigEndian.Uint32(request[12:16]) != transaction {
			return nil, errors.New("malformed announce request")
		}
		record := recordedAnnounce{
			destination:  destination,
			recordedAt:   time.Now(),
			connectionID: binary.BigEndian.Uint64(request[0:8]),
			transaction:  transaction,
			downloaded:   binary.BigEndian.Uint64(request[56:64]),
			left:         binary.BigEndian.Uint64(request[64:72]),
			uploaded:     binary.BigEndian.Uint64(request[72:80]),
			event:        binary.BigEndian.Uint32(request[80:84]),
			ip:           binary.BigEndian.Uint32(request[84:88]),
			key:          binary.BigEndian.Uint32(request[88:92]),
			numWant:      int32(binary.BigEndian.Uint32(request[92:96])),
			port:         binary.BigEndian.Uint16(request[96:98]),
		}
		copy(record.infoHash[:], request[16:36])
		copy(record.peerID[:], request[36:56])
		f.mu.Lock()
		f.announces = append(f.announces, record)
		peers := append([]netip.AddrPort(nil), f.peers[destination]...)
		interval := f.interval
		f.mu.Unlock()
		select {
		case f.announced <- record:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return announceResponsePacket(transaction, interval, !destination.Addr().Unmap().Is4(), peers), nil
	default:
		return nil, fmt.Errorf("unexpected action %d", expectedAction)
	}
}

func (f *fakeTrackerTransport) callCount(address netip.AddrPort) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[address]
}

func (f *fakeTrackerTransport) totalCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.totalCalls
}

func (f *fakeTrackerTransport) connectCount(address netip.AddrPort) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connects[address]
}

func (f *fakeTrackerTransport) snapshotAnnounces() []recordedAnnounce {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedAnnounce(nil), f.announces...)
}

func (f *fakeTrackerTransport) setFailure(address netip.AddrPort, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.failures, address)
		return
	}
	f.failures[address] = err
}

func waitForRetryWait(t *testing.T, retries <-chan recordedRetryWait) recordedRetryWait {
	t.Helper()
	select {
	case retry := <-retries:
		return retry
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for provider retry")
		return recordedRetryWait{}
	}
}

func testAnnouncerTiming() announcerTiming {
	return announcerTiming{
		requestTimeout:     10 * time.Millisecond,
		requestAttempts:    1,
		connectionLifetime: 45 * time.Millisecond,
		resolveTimeout:     20 * time.Millisecond,
		httpRequestTimeout: 200 * time.Millisecond,
		providerRetry:      10 * time.Millisecond,
		providerRetryMax:   20 * time.Millisecond,
		intervalMin:        20 * time.Millisecond,
		intervalMax:        20 * time.Millisecond,
		hintLifetimeMin:    50 * time.Millisecond,
		hintLifetimeMax:    50 * time.Millisecond,
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func waitForAnnounce(t *testing.T, announcements <-chan recordedAnnounce, match func(recordedAnnounce) bool) recordedAnnounce {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case record := <-announcements:
			if match(record) {
				return record
			}
		case <-timer.C:
			t.Fatal("timed out waiting for announce")
		}
	}
}

func waitForRun(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(4 * time.Second):
		t.Fatal("Run did not stop after cancellation")
		return nil
	}
}

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

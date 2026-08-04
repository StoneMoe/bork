package portmap

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/huin/goupnp/soap"
)

var testStart = time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)

var testTokenSeed = []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

const testDescription = "Bork-0102030405060708090a0b0c0d0e0f10"

type externalResult struct {
	address string
	err     error
}

type mappingCall struct {
	externalPort uint16
	internalPort uint16
	internalIP   string
	description  string
	lease        uint32
}

type fakeGateway struct {
	key   string
	name  string
	local net.IP

	mu                  sync.Mutex
	entries             map[uint16]portMappingEntry
	addErrors           []error
	deleteErrors        []error
	externalResults     []externalResult
	addCalls            []mappingCall
	getCalls            []uint16
	deletePorts         []uint16
	deleteContextErrors []error
	externalCalls       int
	addFunc             func(context.Context, int, mappingCall) error
	getFunc             func(context.Context, int, uint16) (portMappingEntry, error)
}

func (g *fakeGateway) id() string        { return g.key }
func (g *fakeGateway) provider() string  { return g.name }
func (g *fakeGateway) localAddr() net.IP { return append(net.IP(nil), g.local...) }

func (g *fakeGateway) addPortMapping(ctx context.Context, externalPort, internalPort uint16, internalIP, description string, lease uint32) error {
	call := mappingCall{
		externalPort: externalPort,
		internalPort: internalPort,
		internalIP:   internalIP,
		description:  description,
		lease:        lease,
	}
	g.mu.Lock()
	g.addCalls = append(g.addCalls, call)
	index := len(g.addCalls) - 1
	addFunc := g.addFunc
	var err error
	if len(g.addErrors) != 0 {
		err = g.addErrors[0]
		g.addErrors = g.addErrors[1:]
	}
	g.mu.Unlock()
	if addFunc != nil {
		return addFunc(ctx, index, call)
	}
	if err != nil {
		return err
	}
	g.setEntry(externalPort, entryFromCall(call))
	return nil
}

func (g *fakeGateway) getSpecificPortMapping(ctx context.Context, externalPort uint16) (portMappingEntry, error) {
	g.mu.Lock()
	g.getCalls = append(g.getCalls, externalPort)
	index := len(g.getCalls) - 1
	getFunc := g.getFunc
	entry, present := g.entries[externalPort]
	g.mu.Unlock()
	if getFunc != nil {
		return getFunc(ctx, index, externalPort)
	}
	if !present {
		return portMappingEntry{}, upnpError(714)
	}
	return entry, nil
}

func (g *fakeGateway) externalIPAddress(_ context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.externalCalls++
	if len(g.externalResults) == 0 {
		return "8.8.8.10", nil
	}
	result := g.externalResults[0]
	g.externalResults = g.externalResults[1:]
	return result.address, result.err
}

func (g *fakeGateway) deletePortMapping(ctx context.Context, externalPort uint16) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deletePorts = append(g.deletePorts, externalPort)
	g.deleteContextErrors = append(g.deleteContextErrors, ctx.Err())
	var err error
	if len(g.deleteErrors) != 0 {
		err = g.deleteErrors[0]
		g.deleteErrors = g.deleteErrors[1:]
	}
	if err == nil || isUPnPError(err, 714) {
		delete(g.entries, externalPort)
	}
	return err
}

func (g *fakeGateway) setEntry(port uint16, entry portMappingEntry) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.entries == nil {
		g.entries = make(map[uint16]portMappingEntry)
	}
	g.entries[port] = entry
}

func (g *fakeGateway) removeEntry(port uint16) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.entries, port)
}

func (g *fakeGateway) entry(port uint16) (portMappingEntry, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	entry, present := g.entries[port]
	return entry, present
}

type gatewaySnapshot struct {
	addCalls            []mappingCall
	getCalls            []uint16
	deletePorts         []uint16
	deleteContextErrors []error
	externalCalls       int
}

func (g *fakeGateway) snapshot() gatewaySnapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	return gatewaySnapshot{
		addCalls:            append([]mappingCall(nil), g.addCalls...),
		getCalls:            append([]uint16(nil), g.getCalls...),
		deletePorts:         append([]uint16(nil), g.deletePorts...),
		deleteContextErrors: append([]error(nil), g.deleteContextErrors...),
		externalCalls:       g.externalCalls,
	}
}

func entryFromCall(call mappingCall) portMappingEntry {
	return portMappingEntry{
		internalPort:  call.internalPort,
		internalIP:    call.internalIP,
		enabled:       true,
		description:   call.description,
		leaseDuration: call.lease,
	}
}

func matchingEntry(port uint16, ip string) portMappingEntry {
	return portMappingEntry{
		internalPort:  port,
		internalIP:    ip,
		enabled:       true,
		description:   testDescription,
		leaseDuration: 10,
	}
}

func foreignEntry() portMappingEntry {
	return portMappingEntry{
		internalPort:  9999,
		internalIP:    "192.168.1.99",
		enabled:       true,
		description:   "Another application",
		leaseDuration: 600,
	}
}

type controlledClock struct {
	mu       sync.Mutex
	current  time.Time
	waits    chan time.Duration
	advances chan time.Duration
}

func newControlledClock() *controlledClock {
	return &controlledClock{
		current:  testStart,
		waits:    make(chan time.Duration),
		advances: make(chan time.Duration),
	}
}

func (clock *controlledClock) now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.current
}

func (clock *controlledClock) elapse(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.current = clock.current.Add(duration)
}

func (clock *controlledClock) wait(ctx context.Context, duration time.Duration) bool {
	select {
	case clock.waits <- duration:
	case <-ctx.Done():
		return false
	}
	select {
	case advance := <-clock.advances:
		clock.elapse(advance)
		return true
	case <-ctx.Done():
		return false
	}
}

func (clock *controlledClock) nextWait(t *testing.T) time.Duration {
	t.Helper()
	select {
	case duration := <-clock.waits:
		return duration
	case <-time.After(2 * time.Second):
		t.Fatal("mapper did not begin waiting")
		return 0
	}
}

func (clock *controlledClock) advance(t *testing.T, duration time.Duration) {
	t.Helper()
	select {
	case clock.advances <- duration:
	case <-time.After(2 * time.Second):
		t.Fatal("mapper was not waiting for a clock advance")
	}
}

func newTestMapper(discover discoverFunc, clock *controlledClock) *UPnP {
	randomBytes := append([]byte(nil), testTokenSeed...)
	randomBytes = append(randomBytes, 0, 1, 0, 2, 0, 3, 0, 4, 0, 5, 0, 6)
	return &UPnP{
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		discover: discover,
		random:   bytes.NewReader(randomBytes),
		now:      clock.now,
		wait:     clock.wait,
		timing: mapperTiming{
			leaseDuration:    10 * time.Second,
			retryInitial:     time.Second,
			retryMaximum:     4 * time.Second,
			discoveryTimeout: time.Minute,
			operationTimeout: time.Minute,
			cleanupTimeout:   time.Second,
		},
	}
}

func oneGateway(router gateway) discoverFunc {
	return func(context.Context) ([]gateway, error) {
		return []gateway{router}, nil
	}
}

func runTestMapper(t *testing.T, mapper *UPnP, port uint16) (context.CancelFunc, <-chan State, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	states := make(chan State, 16)
	done := make(chan error, 1)
	go func() {
		done <- mapper.Run(ctx, port, states)
	}()
	return cancel, states, done
}

func receiveState(t *testing.T, states <-chan State) State {
	t.Helper()
	select {
	case state := <-states:
		return state
	case <-time.After(2 * time.Second):
		t.Fatal("mapper did not publish a state")
		return State{}
	}
}

func finishMapper(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned an error on cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mapper did not stop after cancellation")
	}
}

func requireFailureState(t *testing.T, states <-chan State) State {
	t.Helper()
	state := receiveState(t, states)
	if state.Mapping != nil || state.Error == "" {
		t.Fatalf("failure state = %+v", state)
	}
	return state
}

func TestRunCreatesTokenizedMappingAndCleansUpOwnedRule(t *testing.T) {
	clock := newControlledClock()
	router := &fakeGateway{key: "gateway-v2", name: providerWANIP2, local: net.IPv4(192, 168, 1, 20)}
	mapper := newTestMapper(oneGateway(router), clock)
	cancel, states, done := runTestMapper(t, mapper, 23456)

	state := receiveState(t, states)
	if state.Error != "" || state.Mapping == nil {
		t.Fatalf("successful state = %+v", state)
	}
	if got := state.Mapping.ExternalAddress; got != netip.MustParseAddrPort("8.8.8.10:23456") {
		t.Fatalf("external address = %s", got)
	}
	if !state.Mapping.ExpiresAt.Equal(testStart.Add(10 * time.Second)) {
		t.Fatalf("expiry = %s", state.Mapping.ExpiresAt)
	}
	if delay := clock.nextWait(t); delay != 4*time.Second {
		t.Fatalf("renewal delay = %s, want 4s", delay)
	}
	finishMapper(t, cancel, done)

	snapshot := router.snapshot()
	if len(snapshot.addCalls) != 1 {
		t.Fatalf("AddPortMapping calls = %d, want 1", len(snapshot.addCalls))
	}
	call := snapshot.addCalls[0]
	if call.externalPort != 23456 || call.internalPort != 23456 || call.internalIP != "192.168.1.20" || call.lease != 10 {
		t.Fatalf("AddPortMapping call = %+v", call)
	}
	if call.description != testDescription {
		t.Fatalf("mapping description = %q, want %q", call.description, testDescription)
	}
	for _, character := range []byte(call.description) {
		if character < 0x20 || character > 0x7e {
			t.Fatalf("mapping description contains non-ASCII byte 0x%x", character)
		}
	}
	if len(snapshot.getCalls) != 3 {
		t.Fatalf("ownership queries = %d, want pre-add, post-add, cleanup", len(snapshot.getCalls))
	}
	if len(snapshot.deletePorts) != 1 || snapshot.deletePorts[0] != 23456 {
		t.Fatalf("deleted ports = %v", snapshot.deletePorts)
	}
	if len(snapshot.deleteContextErrors) != 1 || snapshot.deleteContextErrors[0] != nil {
		t.Fatalf("cleanup context errors = %v", snapshot.deleteContextErrors)
	}
}

func TestRunFallsBackWithoutTouchingPreExistingRule(t *testing.T) {
	clock := newControlledClock()
	router := &fakeGateway{key: "gateway", name: providerWANIP1, local: net.IPv4(192, 168, 1, 20)}
	router.setEntry(30000, foreignEntry())
	mapper := newTestMapper(oneGateway(router), clock)
	cancel, states, done := runTestMapper(t, mapper, 30000)

	state := receiveState(t, states)
	if state.Mapping == nil || state.Mapping.ExternalAddress.Port() != 49153 {
		t.Fatalf("fallback mapping = %+v", state.Mapping)
	}
	finishMapper(t, cancel, done)
	snapshot := router.snapshot()
	if len(snapshot.addCalls) != 1 || snapshot.addCalls[0].externalPort != 49153 {
		t.Fatalf("pre-existing rule was touched: %+v", snapshot)
	}
	if len(snapshot.deletePorts) != 1 || snapshot.deletePorts[0] != 49153 {
		t.Fatalf("cleanup deletes = %v, want owned fallback only", snapshot.deletePorts)
	}
	if entry, present := router.entry(30000); !present || entry.description != foreignEntry().description {
		t.Fatalf("foreign entry was changed or removed: %+v, present=%v", entry, present)
	}
}

func TestRunAllowsExactOwnedRule(t *testing.T) {
	clock := newControlledClock()
	router := &fakeGateway{key: "gateway", name: providerWANIP1, local: net.IPv4(10, 0, 0, 8)}
	router.setEntry(31000, matchingEntry(31000, "10.0.0.8"))
	mapper := newTestMapper(oneGateway(router), clock)
	cancel, states, done := runTestMapper(t, mapper, 31000)

	state := receiveState(t, states)
	if state.Mapping == nil || state.Error != "" {
		t.Fatalf("state = %+v", state)
	}
	finishMapper(t, cancel, done)
	snapshot := router.snapshot()
	if len(snapshot.addCalls) != 1 || snapshot.addCalls[0].description != testDescription {
		t.Fatalf("AddPortMapping calls = %+v", snapshot.addCalls)
	}
}

func TestRunReconcilesCommitThenTimeout(t *testing.T) {
	clock := newControlledClock()
	router := &fakeGateway{key: "gateway", name: providerWANIP2, local: net.IPv4(192, 168, 2, 3)}
	router.addFunc = func(_ context.Context, _ int, call mappingCall) error {
		router.setEntry(call.externalPort, entryFromCall(call))
		return context.DeadlineExceeded
	}
	mapper := newTestMapper(oneGateway(router), clock)
	cancel, states, done := runTestMapper(t, mapper, 32000)

	state := receiveState(t, states)
	if state.Mapping == nil || state.Error != "" {
		t.Fatalf("reconciled state = %+v", state)
	}
	finishMapper(t, cancel, done)
	snapshot := router.snapshot()
	if len(snapshot.addCalls) != 1 || len(snapshot.deletePorts) != 1 {
		t.Fatalf("calls after reconciliation = %+v", snapshot)
	}
}

func TestRunAmbiguousTimeoutDoesNotSprayOrDelete(t *testing.T) {
	clock := newControlledClock()
	router := &fakeGateway{
		key:       "gateway",
		name:      providerWANIP2,
		local:     net.IPv4(192, 168, 2, 3),
		addErrors: []error{context.DeadlineExceeded},
	}
	mapper := newTestMapper(oneGateway(router), clock)
	cancel, states, done := runTestMapper(t, mapper, 33000)

	requireFailureState(t, states)
	if delay := clock.nextWait(t); delay != time.Second {
		t.Fatalf("retry delay = %s", delay)
	}
	clock.advance(t, time.Second)
	requireFailureState(t, states)
	clock.nextWait(t)
	finishMapper(t, cancel, done)
	snapshot := router.snapshot()
	if len(snapshot.addCalls) != 1 {
		t.Fatalf("ambiguous Add caused %d attempts, want 1", len(snapshot.addCalls))
	}
	if len(snapshot.deletePorts) != 0 || snapshot.externalCalls != 0 {
		t.Fatalf("ambiguous mapping was deleted or advertised: %+v", snapshot)
	}
	if len(snapshot.getCalls) != 3 {
		t.Fatalf("ownership queries = %d, want pre-add, reconciliation, and pending recheck", len(snapshot.getCalls))
	}
}

func TestRunDoesNotDeleteReplacementDuringCleanup(t *testing.T) {
	clock := newControlledClock()
	router := &fakeGateway{key: "gateway", name: providerWANIP1, local: net.IPv4(192, 168, 3, 2)}
	mapper := newTestMapper(oneGateway(router), clock)
	cancel, states, done := runTestMapper(t, mapper, 34000)
	if state := receiveState(t, states); state.Mapping == nil {
		t.Fatal("mapping was not created")
	}
	router.setEntry(34000, foreignEntry())
	finishMapper(t, cancel, done)
	if deleted := router.snapshot().deletePorts; len(deleted) != 0 {
		t.Fatalf("replacement rule was deleted: %v", deleted)
	}
}

func TestRunTreats714DuringCleanupAsAlreadyGone(t *testing.T) {
	clock := newControlledClock()
	router := &fakeGateway{key: "gateway", name: providerWANIP1, local: net.IPv4(192, 168, 3, 2)}
	mapper := newTestMapper(oneGateway(router), clock)
	cancel, states, done := runTestMapper(t, mapper, 35000)
	if state := receiveState(t, states); state.Mapping == nil {
		t.Fatal("mapping was not created")
	}
	router.removeEntry(35000)
	finishMapper(t, cancel, done)
	if deleted := router.snapshot().deletePorts; len(deleted) != 0 {
		t.Fatalf("already absent rule was deleted: %v", deleted)
	}
}

func TestRunUsesRandomFallbackOnlyForSOAP718(t *testing.T) {
	clock := newControlledClock()
	router := &fakeGateway{
		key:       "gateway",
		name:      providerWANIP1,
		local:     net.IPv4(10, 1, 0, 2),
		addErrors: []error{upnpError(718), nil},
	}
	mapper := newTestMapper(oneGateway(router), clock)
	cancel, states, done := runTestMapper(t, mapper, 36000)

	state := receiveState(t, states)
	if state.Mapping == nil || state.Mapping.ExternalAddress.Port() != 49153 {
		t.Fatalf("fallback mapping = %+v", state.Mapping)
	}
	finishMapper(t, cancel, done)
	snapshot := router.snapshot()
	if len(snapshot.addCalls) != 2 {
		t.Fatalf("AddPortMapping calls = %d, want 2", len(snapshot.addCalls))
	}
	if snapshot.addCalls[0].externalPort != 36000 || snapshot.addCalls[1].externalPort != 49153 {
		t.Fatalf("attempted ports = [%d %d]", snapshot.addCalls[0].externalPort, snapshot.addCalls[1].externalPort)
	}
	for _, call := range snapshot.addCalls {
		if call.lease == 0 || call.description != testDescription {
			t.Fatalf("unsafe fallback call = %+v", call)
		}
	}
}

func TestRunRefusesPermanentOnlyGateway(t *testing.T) {
	clock := newControlledClock()
	router := &fakeGateway{
		key:       "gateway",
		name:      providerWANIP2,
		local:     net.IPv4(10, 2, 0, 2),
		addErrors: []error{upnpError(725)},
	}
	mapper := newTestMapper(oneGateway(router), clock)
	cancel, states, done := runTestMapper(t, mapper, 37000)

	requireFailureState(t, states)
	clock.nextWait(t)
	finishMapper(t, cancel, done)
	snapshot := router.snapshot()
	if len(snapshot.addCalls) != 1 || snapshot.addCalls[0].lease == 0 {
		t.Fatalf("725 Add attempts = %+v", snapshot.addCalls)
	}
	if len(snapshot.deletePorts) != 0 || snapshot.externalCalls != 0 {
		t.Fatalf("725 gateway was treated as owned: %+v", snapshot)
	}
}

func TestRunRejectsReportedPermanentLeaseAndDeletesOwnedRule(t *testing.T) {
	clock := newControlledClock()
	router := &fakeGateway{key: "gateway", name: providerWANIP2, local: net.IPv4(10, 2, 0, 3)}
	router.addFunc = func(_ context.Context, _ int, call mappingCall) error {
		entry := entryFromCall(call)
		entry.leaseDuration = 0
		router.setEntry(call.externalPort, entry)
		return nil
	}
	mapper := newTestMapper(oneGateway(router), clock)
	cancel, states, done := runTestMapper(t, mapper, 37100)

	state := requireFailureState(t, states)
	if !strings.Contains(state.Error, "lease duration 0") {
		t.Fatalf("error = %q", state.Error)
	}
	clock.nextWait(t)
	finishMapper(t, cancel, done)
	snapshot := router.snapshot()
	if len(snapshot.addCalls) != 1 || len(snapshot.getCalls) != 3 || snapshot.externalCalls != 0 {
		t.Fatalf("permanent lease verification calls = %+v", snapshot)
	}
	if len(snapshot.deletePorts) != 1 || snapshot.deletePorts[0] != 37100 {
		t.Fatalf("permanent lease cleanup deletes = %v", snapshot.deletePorts)
	}
	if _, present := router.entry(37100); present {
		t.Fatal("token-matched permanent mapping remained after cleanup")
	}
}

func TestRunTreatsDefinitiveSOAPFaultAsGatewayFailure(t *testing.T) {
	clock := newControlledClock()
	unsupported := &fakeGateway{
		key:       "unsupported",
		name:      providerWANIP2,
		local:     net.IPv4(10, 2, 0, 2),
		addErrors: []error{upnpError(606)},
	}
	unsupported.getFunc = func(_ context.Context, index int, _ uint16) (portMappingEntry, error) {
		if index == 0 {
			return portMappingEntry{}, upnpError(714)
		}
		return portMappingEntry{}, errors.New("ownership query unavailable")
	}
	working := &fakeGateway{key: "working", name: providerWANIP1, local: net.IPv4(10, 2, 0, 3)}
	mapper := newTestMapper(func(context.Context) ([]gateway, error) {
		return []gateway{unsupported, working}, nil
	}, clock)
	cancel, states, done := runTestMapper(t, mapper, 37500)

	state := receiveState(t, states)
	if state.Mapping == nil || state.Mapping.Provider != providerWANIP1 {
		t.Fatalf("selected mapping = %+v", state.Mapping)
	}
	finishMapper(t, cancel, done)
	if calls := unsupported.snapshot().addCalls; len(calls) != 1 {
		t.Fatalf("unsupported gateway Add calls = %d, want 1", len(calls))
	}
	if calls := working.snapshot().addCalls; len(calls) != 1 {
		t.Fatalf("working gateway Add calls = %d, want 1", len(calls))
	}
}

func TestRunRollsBackAfterExternalIPFailure(t *testing.T) {
	clock := newControlledClock()
	router := &fakeGateway{
		key:             "gateway",
		name:            providerWANIP1,
		local:           net.IPv4(10, 3, 0, 2),
		externalResults: []externalResult{{err: errors.New("external IP unavailable")}},
	}
	mapper := newTestMapper(oneGateway(router), clock)
	cancel, states, done := runTestMapper(t, mapper, 38000)

	requireFailureState(t, states)
	clock.nextWait(t)
	finishMapper(t, cancel, done)
	snapshot := router.snapshot()
	if len(snapshot.getCalls) != 3 {
		t.Fatalf("ownership queries = %d, want pre-add, post-add, rollback", len(snapshot.getCalls))
	}
	if len(snapshot.deletePorts) != 1 || snapshot.deletePorts[0] != 38000 {
		t.Fatalf("rollback deletes = %v", snapshot.deletePorts)
	}
}

func TestRunRenewsWithOwnershipChecksAndConservativeExpiry(t *testing.T) {
	clock := newControlledClock()
	router := &fakeGateway{
		key:   "gateway",
		name:  providerWANIP1,
		local: net.IPv4(192, 168, 50, 2),
		externalResults: []externalResult{
			{address: "1.1.1.4"},
			{address: "1.1.1.5"},
		},
	}
	mapper := newTestMapper(oneGateway(router), clock)
	cancel, states, done := runTestMapper(t, mapper, 39000)

	first := receiveState(t, states)
	if first.Mapping == nil {
		t.Fatal("initial state has no mapping")
	}
	if delay := clock.nextWait(t); delay != 4*time.Second {
		t.Fatalf("renewal delay = %s", delay)
	}
	clock.advance(t, 4*time.Second)
	second := receiveState(t, states)
	if second.Mapping == nil || second.Error != "" {
		t.Fatalf("renewed state = %+v", second)
	}
	if got := second.Mapping.ExternalAddress; got != netip.MustParseAddrPort("1.1.1.5:39000") {
		t.Fatalf("renewed address = %s", got)
	}
	if !second.Mapping.ExpiresAt.Equal(testStart.Add(14 * time.Second)) {
		t.Fatalf("renewed expiry = %s", second.Mapping.ExpiresAt)
	}
	finishMapper(t, cancel, done)
	snapshot := router.snapshot()
	if len(snapshot.addCalls) != 2 || len(snapshot.getCalls) != 5 {
		t.Fatalf("renewal calls = %+v", snapshot)
	}
}

func TestRunRejectsZeroLeaseRenewalWithoutDeletingReplacement(t *testing.T) {
	clock := newControlledClock()
	router := &fakeGateway{key: "gateway", name: providerWANIP1, local: net.IPv4(192, 168, 50, 7)}
	router.addFunc = func(_ context.Context, index int, call mappingCall) error {
		entry := entryFromCall(call)
		if index == 1 {
			entry.leaseDuration = 0
		}
		router.setEntry(call.externalPort, entry)
		return nil
	}
	router.getFunc = func(_ context.Context, index int, port uint16) (portMappingEntry, error) {
		if index == 4 {
			replacement := foreignEntry()
			router.setEntry(port, replacement)
			return replacement, nil
		}
		if entry, present := router.entry(port); present {
			return entry, nil
		}
		return portMappingEntry{}, upnpError(714)
	}
	mapper := newTestMapper(oneGateway(router), clock)
	cancel, states, done := runTestMapper(t, mapper, 39500)

	if state := receiveState(t, states); state.Mapping == nil {
		t.Fatal("initial finite mapping was not created")
	}
	clock.nextWait(t)
	clock.advance(t, 4*time.Second)
	state := requireFailureState(t, states)
	if !strings.Contains(state.Error, "lease duration 0") {
		t.Fatalf("renewal error = %q", state.Error)
	}
	clock.nextWait(t)
	finishMapper(t, cancel, done)
	snapshot := router.snapshot()
	if len(snapshot.addCalls) != 2 || len(snapshot.getCalls) != 5 || snapshot.externalCalls != 1 {
		t.Fatalf("zero lease renewal calls = %+v", snapshot)
	}
	if len(snapshot.deletePorts) != 0 {
		t.Fatalf("replacement mapping was deleted: %v", snapshot.deletePorts)
	}
}

func TestRunRetainsCandidateAcrossIndeterminatePreRenewChecksUntilExpiry(t *testing.T) {
	clock := newControlledClock()
	router := &fakeGateway{key: "gateway", name: providerWANIP1, local: net.IPv4(192, 168, 50, 3)}
	router.getFunc = func(_ context.Context, index int, port uint16) (portMappingEntry, error) {
		if index == 2 || index == 3 {
			return portMappingEntry{}, errors.New("ownership query timed out")
		}
		if entry, present := router.entry(port); present {
			return entry, nil
		}
		return portMappingEntry{}, upnpError(714)
	}
	var discoveries atomic.Int32
	mapper := newTestMapper(func(ctx context.Context) ([]gateway, error) {
		if discoveries.Add(1) == 1 {
			return []gateway{router}, nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}, clock)
	cancel, states, done := runTestMapper(t, mapper, 39100)

	initial := receiveState(t, states)
	if initial.Mapping == nil {
		t.Fatal("initial state has no mapping")
	}
	clock.nextWait(t)
	clock.advance(t, 4*time.Second)
	firstFailure := receiveState(t, states)
	if firstFailure.Mapping == nil || firstFailure.Error == "" || !firstFailure.Mapping.ExpiresAt.Equal(initial.Mapping.ExpiresAt) {
		t.Fatalf("first indeterminate state = %+v", firstFailure)
	}
	if delay := clock.nextWait(t); delay != time.Second {
		t.Fatalf("first retry delay = %s", delay)
	}
	clock.advance(t, time.Second)
	secondFailure := receiveState(t, states)
	if secondFailure.Mapping == nil || secondFailure.Error == "" || !secondFailure.Mapping.ExpiresAt.Equal(initial.Mapping.ExpiresAt) {
		t.Fatalf("second indeterminate state = %+v", secondFailure)
	}
	if got := len(router.snapshot().addCalls); got != 1 {
		t.Fatalf("Add calls before expiry = %d, want initial Add only", got)
	}
	if got := discoveries.Load(); got != 1 {
		t.Fatalf("discoveries before expiry = %d, want 1", got)
	}
	if delay := clock.nextWait(t); delay != 2*time.Second {
		t.Fatalf("second retry delay = %s", delay)
	}
	clock.advance(t, 5*time.Second)
	if expired := receiveState(t, states); expired.Mapping != nil {
		t.Fatalf("expired candidate remained advertised: %+v", expired.Mapping)
	}
	finishMapper(t, cancel, done)
	if deleted := router.snapshot().deletePorts; len(deleted) != 1 || deleted[0] != 39100 {
		t.Fatalf("expiry cleanup deletes = %v", deleted)
	}
}

func TestRunRetainsOldLeaseWhenPostRenewVerificationIsIndeterminate(t *testing.T) {
	clock := newControlledClock()
	router := &fakeGateway{key: "gateway", name: providerWANIP1, local: net.IPv4(192, 168, 50, 4)}
	router.getFunc = func(_ context.Context, index int, port uint16) (portMappingEntry, error) {
		if index == 3 {
			return portMappingEntry{}, errors.New("post-Add ownership query timed out")
		}
		if entry, present := router.entry(port); present {
			return entry, nil
		}
		return portMappingEntry{}, upnpError(714)
	}
	var discoveries atomic.Int32
	mapper := newTestMapper(func(context.Context) ([]gateway, error) {
		discoveries.Add(1)
		return []gateway{router}, nil
	}, clock)
	cancel, states, done := runTestMapper(t, mapper, 39200)

	initial := receiveState(t, states)
	clock.nextWait(t)
	clock.advance(t, 4*time.Second)
	failed := receiveState(t, states)
	if failed.Mapping == nil || failed.Error == "" || !failed.Mapping.ExpiresAt.Equal(initial.Mapping.ExpiresAt) {
		t.Fatalf("indeterminate post-Add state = %+v", failed)
	}
	if got := len(router.snapshot().addCalls); got != 2 {
		t.Fatalf("Add calls = %d, want initial and one renewal", got)
	}
	if got := discoveries.Load(); got != 1 {
		t.Fatalf("discoveries = %d, want 1", got)
	}
	finishMapper(t, cancel, done)
}

func TestRunRetainsExactRuleWhenRenewAddReturnsError(t *testing.T) {
	clock := newControlledClock()
	router := &fakeGateway{
		key:       "gateway",
		name:      providerWANIP1,
		local:     net.IPv4(192, 168, 50, 5),
		addErrors: []error{nil, upnpError(725)},
	}
	mapper := newTestMapper(oneGateway(router), clock)
	cancel, states, done := runTestMapper(t, mapper, 39300)

	initial := receiveState(t, states)
	clock.nextWait(t)
	clock.advance(t, 4*time.Second)
	failed := receiveState(t, states)
	if failed.Mapping == nil || failed.Error == "" || !failed.Mapping.ExpiresAt.Equal(initial.Mapping.ExpiresAt) {
		t.Fatalf("failed Add renewal state = %+v", failed)
	}
	if deleted := router.snapshot().deletePorts; len(deleted) != 0 {
		t.Fatalf("exact live rule was rolled back: %v", deleted)
	}
	finishMapper(t, cancel, done)
	if deleted := router.snapshot().deletePorts; len(deleted) != 1 || deleted[0] != 39300 {
		t.Fatalf("cancellation cleanup deletes = %v", deleted)
	}
}

func TestRunRetainsRenewedMappingWhenExternalIPRefreshFails(t *testing.T) {
	clock := newControlledClock()
	router := &fakeGateway{
		key:   "gateway",
		name:  providerWANIP1,
		local: net.IPv4(192, 168, 50, 6),
		externalResults: []externalResult{
			{address: "8.8.8.6"},
			{err: errors.New("external IP query timed out")},
		},
	}
	mapper := newTestMapper(oneGateway(router), clock)
	cancel, states, done := runTestMapper(t, mapper, 39400)

	initial := receiveState(t, states)
	clock.nextWait(t)
	clock.advance(t, 4*time.Second)
	failed := receiveState(t, states)
	if failed.Mapping == nil || failed.Error == "" {
		t.Fatalf("external-IP renewal failure state = %+v", failed)
	}
	if failed.Mapping.ExternalAddress != initial.Mapping.ExternalAddress {
		t.Fatalf("external address changed from %s to %s", initial.Mapping.ExternalAddress, failed.Mapping.ExternalAddress)
	}
	if !failed.Mapping.ExpiresAt.Equal(testStart.Add(14 * time.Second)) {
		t.Fatalf("renewed expiry = %s", failed.Mapping.ExpiresAt)
	}
	if deleted := router.snapshot().deletePorts; len(deleted) != 0 {
		t.Fatalf("renewed mapping was deleted after external-IP failure: %v", deleted)
	}
	finishMapper(t, cancel, done)
}

func TestRunLeaseStartsBeforeAddCompletes(t *testing.T) {
	clock := newControlledClock()
	router := &fakeGateway{key: "gateway", name: providerWANIP1, local: net.IPv4(192, 168, 4, 2)}
	router.addFunc = func(_ context.Context, _ int, call mappingCall) error {
		clock.elapse(3 * time.Second)
		router.setEntry(call.externalPort, entryFromCall(call))
		return nil
	}
	mapper := newTestMapper(oneGateway(router), clock)
	cancel, states, done := runTestMapper(t, mapper, 40000)

	state := receiveState(t, states)
	if state.Mapping == nil || !state.Mapping.ExpiresAt.Equal(testStart.Add(10*time.Second)) {
		t.Fatalf("conservative mapping = %+v", state.Mapping)
	}
	if delay := clock.nextWait(t); delay != time.Second {
		t.Fatalf("post-operation renewal delay = %s, want 1s", delay)
	}
	finishMapper(t, cancel, done)
}

func TestRunCleansOwnedRuleAtConservativeExpiry(t *testing.T) {
	clock := newControlledClock()
	router := &fakeGateway{key: "gateway", name: providerWANIP1, local: net.IPv4(192, 168, 4, 3)}
	var discoveries atomic.Int32
	mapper := newTestMapper(func(ctx context.Context) ([]gateway, error) {
		if discoveries.Add(1) == 1 {
			return []gateway{router}, nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}, clock)
	cancel, states, done := runTestMapper(t, mapper, 40500)

	if state := receiveState(t, states); state.Mapping == nil {
		t.Fatal("initial state has no mapping")
	}
	if delay := clock.nextWait(t); delay != 4*time.Second {
		t.Fatalf("renewal delay = %s", delay)
	}
	// Simulate a stalled process waking after its conservative lease bound while
	// the router still retains the tokenized rule.
	clock.advance(t, 10*time.Second)
	if state := receiveState(t, states); state.Mapping != nil {
		t.Fatalf("expired state retained mapping %+v", state.Mapping)
	}
	finishMapper(t, cancel, done)

	snapshot := router.snapshot()
	if len(snapshot.deletePorts) != 1 || snapshot.deletePorts[0] != 40500 {
		t.Fatalf("expiry cleanup deletes = %v", snapshot.deletePorts)
	}
	if _, present := router.entry(40500); present {
		t.Fatal("owned rule remained after conservative expiry cleanup")
	}
}

func TestRunInvalidatesMappingWhenRefreshOwnershipIsLost(t *testing.T) {
	clock := newControlledClock()
	router := &fakeGateway{key: "gateway", name: providerWANIP1, local: net.IPv4(192, 168, 5, 2)}
	mapper := newTestMapper(oneGateway(router), clock)
	cancel, states, done := runTestMapper(t, mapper, 41000)

	if state := receiveState(t, states); state.Mapping == nil {
		t.Fatal("initial state has no mapping")
	}
	clock.nextWait(t)
	router.setEntry(41000, foreignEntry())
	clock.advance(t, 4*time.Second)
	failed := requireFailureState(t, states)
	if !strings.Contains(failed.Error, "ownership") {
		t.Fatalf("error = %q", failed.Error)
	}
	finishMapper(t, cancel, done)
	snapshot := router.snapshot()
	if len(snapshot.addCalls) != 1 || len(snapshot.deletePorts) != 0 {
		t.Fatalf("replacement was renewed or deleted: %+v", snapshot)
	}
}

func TestRunDiscoveryFailuresUseBoundedExponentialBackoff(t *testing.T) {
	clock := newControlledClock()
	var discoveries atomic.Int32
	mapper := newTestMapper(func(context.Context) ([]gateway, error) {
		discoveries.Add(1)
		return nil, errors.New(strings.Repeat("router failure ", 200))
	}, clock)
	cancel, states, done := runTestMapper(t, mapper, 42000)

	for index, wantDelay := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second} {
		state := requireFailureState(t, states)
		if len(state.Error) > maxErrorLength {
			t.Fatalf("error length = %d", len(state.Error))
		}
		if delay := clock.nextWait(t); delay != wantDelay {
			t.Fatalf("retry delay %d = %s, want %s", index, delay, wantDelay)
		}
		if index != 3 {
			clock.advance(t, wantDelay)
		}
	}
	if got := discoveries.Load(); got != 4 {
		t.Fatalf("discoveries = %d, want 4", got)
	}
	finishMapper(t, cancel, done)
}

func TestRunRejectsInvalidArgumentsWithoutDiscovery(t *testing.T) {
	var discoveries atomic.Int32
	mapper := NewUPnP(nil).(*UPnP)
	mapper.discover = func(context.Context) ([]gateway, error) {
		discoveries.Add(1)
		return nil, nil
	}
	states := make(chan State)

	if err := mapper.Run(nil, 1234, states); err == nil {
		t.Fatal("nil context was accepted")
	}
	if err := mapper.Run(context.Background(), 0, states); err == nil {
		t.Fatal("zero port was accepted")
	}
	if err := mapper.Run(context.Background(), 1234, nil); err == nil {
		t.Fatal("nil state channel was accepted")
	}
	if got := discoveries.Load(); got != 0 {
		t.Fatalf("invalid runs performed %d discoveries", got)
	}
}

func TestAddressValidation(t *testing.T) {
	valid, err := validLocalAddress(net.IPv4(192, 168, 1, 5))
	if err != nil || valid != netip.MustParseAddr("192.168.1.5") {
		t.Fatalf("valid local address = %s, %v", valid, err)
	}
	for _, address := range []net.IP{nil, net.IPv6loopback, net.IPv4zero, net.IPv4(127, 0, 0, 1), net.IPv4(169, 254, 1, 1), net.IPv4(224, 0, 0, 1), net.IPv4bcast} {
		if _, err := validLocalAddress(address); err == nil {
			t.Fatalf("invalid local address %v was accepted", address)
		}
	}

	for _, address := range []string{
		"",
		"not-an-ip",
		"0.0.0.1",
		"10.0.0.1",
		"100.64.0.1",
		"100.127.255.254",
		"127.0.0.1",
		"169.254.1.1",
		"172.16.0.1",
		"172.31.255.254",
		"192.0.0.1",
		"192.0.2.1",
		"192.88.99.1",
		"192.168.1.1",
		"198.18.0.1",
		"198.19.255.254",
		"198.51.100.1",
		"203.0.113.1",
		"224.0.0.1",
		"239.255.255.255",
		"240.0.0.1",
		"255.255.255.255",
		"2001:db8::1",
	} {
		router := &fakeGateway{externalResults: []externalResult{{address: address}}}
		if _, err := queryExternalAddress(context.Background(), router); err == nil {
			t.Fatalf("invalid external address %q was accepted", address)
		}
	}
	for _, address := range []string{"1.1.1.1", "8.8.8.8", "9.9.9.9"} {
		router := &fakeGateway{externalResults: []externalResult{{address: address}}}
		got, err := queryExternalAddress(context.Background(), router)
		if err != nil || got.String() != address {
			t.Fatalf("public external address %q = %s, %v", address, got, err)
		}
	}
}

func TestBoundedTextPreservesUTF8(t *testing.T) {
	text := boundedText(strings.Repeat("é", maxErrorLength), maxErrorLength)
	if len(text) > maxErrorLength || !strings.HasSuffix(text, "...") {
		t.Fatalf("bounded text length = %d, value = %q", len(text), text)
	}
}

func upnpError(code int) error {
	fault := &soap.SOAPFaultError{}
	fault.Detail.UPnPError.Errorcode = code
	return fault
}

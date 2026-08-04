package portmap

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type natPMPExternalOutcome struct {
	result natPMPExternalResult
	err    error
}

type natPMPPortOutcome struct {
	result natPMPPortResult
	err    error
}

type natPMPCall struct {
	internalPort uint16
	externalPort uint16
	lifetime     uint32
}

type fakeNATPMPClient struct {
	mu               sync.Mutex
	externalOutcomes []natPMPExternalOutcome
	portOutcomes     []natPMPPortOutcome
	operations       []string
	portCalls        []natPMPCall
}

func (client *fakeNATPMPClient) externalAddress() (natPMPExternalResult, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.operations = append(client.operations, "external")
	if len(client.externalOutcomes) == 0 {
		return natPMPExternalResult{address: netip.MustParseAddr("8.8.8.8"), epoch: 100}, nil
	}
	outcome := client.externalOutcomes[0]
	client.externalOutcomes = client.externalOutcomes[1:]
	return outcome.result, outcome.err
}

func (client *fakeNATPMPClient) addUDPMapping(internalPort, externalPort uint16, lifetime uint32) (natPMPPortResult, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.operations = append(client.operations, "add")
	client.portCalls = append(client.portCalls, natPMPCall{
		internalPort: internalPort,
		externalPort: externalPort,
		lifetime:     lifetime,
	})
	if len(client.portOutcomes) != 0 {
		outcome := client.portOutcomes[0]
		client.portOutcomes = client.portOutcomes[1:]
		return outcome.result, outcome.err
	}
	if lifetime == 0 {
		return natPMPPortResult{epoch: 110, internalPort: internalPort}, nil
	}
	return natPMPPortResult{
		epoch:        101,
		internalPort: internalPort,
		externalPort: externalPort,
		lifetime:     lifetime,
	}, nil
}

type natPMPSnapshot struct {
	operations []string
	portCalls  []natPMPCall
}

func (client *fakeNATPMPClient) snapshot() natPMPSnapshot {
	client.mu.Lock()
	defer client.mu.Unlock()
	return natPMPSnapshot{
		operations: append([]string(nil), client.operations...),
		portCalls:  append([]natPMPCall(nil), client.portCalls...),
	}
}

func newTestNATPMP(client natPMPClient, clock *controlledClock) *NATPMP {
	return &NATPMP{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		discover: func(context.Context) ([]net.IP, error) {
			return []net.IP{net.IPv4(192, 168, 1, 1)}, nil
		},
		newClient: func(net.IP, time.Duration) natPMPClient {
			return client
		},
		now:  clock.now,
		wait: clock.wait,
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

func runNATPMPMapper(t *testing.T, mapper *NATPMP, port uint16) (context.CancelFunc, <-chan State, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	states := make(chan State, 16)
	done := make(chan error, 1)
	go func() {
		done <- mapper.Run(ctx, port, states)
	}()
	return cancel, states, done
}

func TestNATPMPRenewalRequeriesAndUpdatesPublicIPv4(t *testing.T) {
	clock := newControlledClock()
	client := &fakeNATPMPClient{
		externalOutcomes: []natPMPExternalOutcome{
			{result: natPMPExternalResult{address: netip.MustParseAddr("1.1.1.4"), epoch: 100}},
			{result: natPMPExternalResult{address: netip.MustParseAddr("8.8.8.8"), epoch: 104}},
		},
		portOutcomes: []natPMPPortOutcome{
			{result: natPMPPortResult{epoch: 101, internalPort: 40000, externalPort: 45000, lifetime: 10}},
			{result: natPMPPortResult{epoch: 105, internalPort: 40000, externalPort: 45001, lifetime: 10}},
		},
	}
	mapper := newTestNATPMP(client, clock)
	cancel, states, done := runNATPMPMapper(t, mapper, 40000)

	initial := receiveState(t, states)
	if initial.Mapping == nil || initial.Error != "" {
		t.Fatalf("initial state = %+v", initial)
	}
	if initial.Mapping.ExternalAddress != netip.MustParseAddrPort("1.1.1.4:45000") {
		t.Fatalf("initial mapping = %+v", initial.Mapping)
	}
	if delay := clock.nextWait(t); delay != 4*time.Second {
		t.Fatalf("renewal delay = %s, want 4s", delay)
	}
	clock.advance(t, 4*time.Second)

	renewed := receiveState(t, states)
	if renewed.Mapping == nil || renewed.Error != "" {
		t.Fatalf("renewed state = %+v", renewed)
	}
	if renewed.Mapping.ExternalAddress != netip.MustParseAddrPort("8.8.8.8:45001") {
		t.Fatalf("renewed address = %s", renewed.Mapping.ExternalAddress)
	}
	if !renewed.Mapping.ExpiresAt.Equal(testStart.Add(14 * time.Second)) {
		t.Fatalf("renewed expiry = %s", renewed.Mapping.ExpiresAt)
	}
	beforeCleanup := client.snapshot()
	if !slices.Equal(beforeCleanup.operations, []string{"external", "add", "external", "add"}) {
		t.Fatalf("operation order = %v", beforeCleanup.operations)
	}
	if len(beforeCleanup.portCalls) != 2 {
		t.Fatalf("mapping calls = %+v", beforeCleanup.portCalls)
	}
	if call := beforeCleanup.portCalls[1]; call.externalPort != 45000 || call.lifetime != 10 {
		t.Fatalf("renewal call = %+v", call)
	}

	finishMapper(t, cancel, done)
	afterCleanup := client.snapshot()
	if len(afterCleanup.portCalls) != 3 {
		t.Fatalf("calls after cleanup = %+v", afterCleanup.portCalls)
	}
	if call := afterCleanup.portCalls[2]; call.internalPort != 40000 || call.externalPort != 0 || call.lifetime != 0 {
		t.Fatalf("cleanup call = %+v", call)
	}
}

func TestNATPMPDoesNotRenewWithoutFreshPublicIPv4(t *testing.T) {
	clock := newControlledClock()
	client := &fakeNATPMPClient{
		externalOutcomes: []natPMPExternalOutcome{
			{result: natPMPExternalResult{address: netip.MustParseAddr("8.8.4.4"), epoch: 100}},
			{err: errors.New("address query timed out")},
		},
		portOutcomes: []natPMPPortOutcome{{
			result: natPMPPortResult{epoch: 101, internalPort: 41000, externalPort: 46000, lifetime: 10},
		}},
	}
	mapper := newTestNATPMP(client, clock)
	cancel, states, done := runNATPMPMapper(t, mapper, 41000)

	initial := receiveState(t, states)
	clock.nextWait(t)
	clock.advance(t, 4*time.Second)
	failed := receiveState(t, states)
	if failed.Mapping == nil || failed.Error == "" {
		t.Fatalf("failed renewal state = %+v", failed)
	}
	if failed.Mapping.ExternalAddress != initial.Mapping.ExternalAddress || !failed.Mapping.ExpiresAt.Equal(initial.Mapping.ExpiresAt) {
		t.Fatalf("failed renewal changed mapping from %+v to %+v", initial.Mapping, failed.Mapping)
	}
	if !strings.Contains(failed.Error, "before NAT-PMP renewal") {
		t.Fatalf("renewal error = %q", failed.Error)
	}
	snapshot := client.snapshot()
	if !slices.Equal(snapshot.operations, []string{"external", "add", "external"}) {
		t.Fatalf("operations after address failure = %v", snapshot.operations)
	}
	if len(snapshot.portCalls) != 1 {
		t.Fatalf("renewal mapping was attempted: %+v", snapshot.portCalls)
	}

	finishMapper(t, cancel, done)
}

func TestNATPMPRejectsNonPublicAddressBeforeMapping(t *testing.T) {
	clock := newControlledClock()
	client := &fakeNATPMPClient{
		externalOutcomes: []natPMPExternalOutcome{{
			result: natPMPExternalResult{address: netip.MustParseAddr("192.168.1.20"), epoch: 100},
		}},
	}
	mapper := newTestNATPMP(client, clock)
	cancel, states, done := runNATPMPMapper(t, mapper, 42000)

	state := requireFailureState(t, states)
	if !strings.Contains(state.Error, "not public IPv4") {
		t.Fatalf("error = %q", state.Error)
	}
	if calls := client.snapshot().portCalls; len(calls) != 0 {
		t.Fatalf("mapping calls = %+v", calls)
	}
	clock.nextWait(t)
	finishMapper(t, cancel, done)
}

func TestNATPMPAmbiguousAddIsNotAdvertisedOrDeleted(t *testing.T) {
	clock := newControlledClock()
	client := &fakeNATPMPClient{
		externalOutcomes: []natPMPExternalOutcome{{
			result: natPMPExternalResult{address: netip.MustParseAddr("9.9.9.9"), epoch: 100},
		}},
		portOutcomes: []natPMPPortOutcome{{err: errors.New("mapping response timed out")}},
	}
	mapper := newTestNATPMP(client, clock)
	cancel, states, done := runNATPMPMapper(t, mapper, 43000)

	state := requireFailureState(t, states)
	if !strings.Contains(state.Error, "mapping response timed out") {
		t.Fatalf("error = %q", state.Error)
	}
	clock.nextWait(t)
	finishMapper(t, cancel, done)

	calls := client.snapshot().portCalls
	if len(calls) != 1 {
		t.Fatalf("mapping calls = %+v", calls)
	}
	if calls[0].lifetime != 10 {
		t.Fatalf("mapping call = %+v", calls)
	}
}

func TestNATPMPRejectsInvalidMappingResponses(t *testing.T) {
	tests := []struct {
		name   string
		result natPMPPortResult
		want   string
	}{
		{name: "internal port", result: natPMPPortResult{epoch: 101, internalPort: 1, externalPort: 45000, lifetime: 10}, want: "internal port"},
		{name: "external port", result: natPMPPortResult{epoch: 101, internalPort: 44000, lifetime: 10}, want: "zero external port"},
		{name: "lifetime", result: natPMPPortResult{epoch: 101, internalPort: 44000, externalPort: 45000}, want: "zero mapping lifetime"},
		{name: "restart", result: natPMPPortResult{epoch: 99, internalPort: 44000, externalPort: 45000, lifetime: 10}, want: "restarted"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			owned := newNATPMPMapping(netip.MustParseAddr("192.168.1.1"), 44000, netip.MustParseAddr("8.8.8.8"), 10, testStart)
			err := applyNATPMPResult(owned, natPMPExternalResult{address: netip.MustParseAddr("8.8.8.8"), epoch: 100}, test.result, 10, testStart)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("apply result error = %v, want %q", err, test.want)
			}
			if owned.advertised {
				t.Fatal("invalid response was advertised")
			}
		})
	}
}

func TestNATPMPRejectsInvalidArgumentsWithoutDiscovery(t *testing.T) {
	mapper := NewNATPMP(nil).(*NATPMP)
	discoveries := 0
	mapper.discover = func(context.Context) ([]net.IP, error) {
		discoveries++
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
	if discoveries != 0 {
		t.Fatalf("invalid runs performed %d discoveries", discoveries)
	}
}

func TestValidNATPMPGateway(t *testing.T) {
	got, err := validNATPMPGateway(net.IPv4(192, 168, 1, 1))
	if err != nil || got != netip.MustParseAddr("192.168.1.1") {
		t.Fatalf("gateway = %s, %v", got, err)
	}
	for _, address := range []net.IP{nil, net.IPv6loopback, net.IPv4zero, net.IPv4(127, 0, 0, 1), net.IPv4(169, 254, 1, 1), net.IPv4(224, 0, 0, 1)} {
		if _, err := validNATPMPGateway(address); err == nil {
			t.Fatalf("invalid gateway %v was accepted", address)
		}
	}
}

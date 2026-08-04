package portmap

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type mapperFunc func(context.Context, uint16, chan<- State) error

func (function mapperFunc) Run(ctx context.Context, port uint16, states chan<- State) error {
	return function(ctx, port, states)
}

func runGatewayMapper(t *testing.T, mapper *Gateway, port uint16) (context.CancelFunc, <-chan State, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	states := make(chan State, 16)
	done := make(chan error, 1)
	go func() {
		done <- mapper.Run(ctx, port, states)
	}()
	return cancel, states, done
}

func TestGatewayFallsBackToNextProvider(t *testing.T) {
	firstStopped := make(chan struct{})
	secondStopped := make(chan struct{})
	first := mapperFunc(func(ctx context.Context, _ uint16, states chan<- State) error {
		states <- State{Error: "UPnP unavailable"}
		<-ctx.Done()
		close(firstStopped)
		return nil
	})
	second := mapperFunc(func(ctx context.Context, port uint16, states chan<- State) error {
		mapping := Mapping{
			ExternalAddress: netip.AddrPortFrom(netip.MustParseAddr("8.8.8.8"), port+1),
			Provider:        providerNATPMP,
			ExpiresAt:       time.Now().Add(time.Hour),
		}
		states <- State{Mapping: &mapping}
		<-ctx.Done()
		close(secondStopped)
		return nil
	})
	mapper := &Gateway{
		providers: []mapperProvider{
			{name: "UPnP", mapper: first},
			{name: providerNATPMP, mapper: second},
		},
		wait: waitContext,
	}
	cancel, states, done := runGatewayMapper(t, mapper, 42000)

	state := receiveState(t, states)
	if state.Mapping == nil || state.Error != "" {
		t.Fatalf("fallback state = %+v", state)
	}
	if state.Mapping.Provider != providerNATPMP || state.Mapping.ExternalAddress.Port() != 42001 {
		t.Fatalf("fallback mapping = %+v", state.Mapping)
	}
	waitForTestSignal(t, firstStopped, "first provider shutdown")

	finishMapper(t, cancel, done)
	waitForTestSignal(t, secondStopped, "second provider shutdown")
}

func TestGatewayFailsOverAfterActiveMappingIsLost(t *testing.T) {
	loseFirst := make(chan struct{})
	firstStopped := make(chan struct{})
	first := mapperFunc(func(ctx context.Context, port uint16, states chan<- State) error {
		mapping := Mapping{
			ExternalAddress: netip.AddrPortFrom(netip.MustParseAddr("1.1.1.1"), port),
			Provider:        "first",
			ExpiresAt:       time.Now().Add(time.Hour),
		}
		states <- State{Mapping: &mapping}
		select {
		case <-loseFirst:
			states <- State{Error: "first mapping expired"}
		case <-ctx.Done():
			close(firstStopped)
			return nil
		}
		<-ctx.Done()
		close(firstStopped)
		return nil
	})
	second := mapperFunc(func(ctx context.Context, port uint16, states chan<- State) error {
		mapping := Mapping{
			ExternalAddress: netip.AddrPortFrom(netip.MustParseAddr("8.8.8.8"), port+2),
			Provider:        "second",
			ExpiresAt:       time.Now().Add(time.Hour),
		}
		states <- State{Mapping: &mapping}
		<-ctx.Done()
		return nil
	})
	mapper := &Gateway{
		providers: []mapperProvider{
			{name: "first", mapper: first},
			{name: "second", mapper: second},
		},
		wait: waitContext,
	}
	cancel, states, done := runGatewayMapper(t, mapper, 43000)

	initial := receiveState(t, states)
	if initial.Mapping == nil || initial.Mapping.Provider != "first" {
		t.Fatalf("initial state = %+v", initial)
	}
	close(loseFirst)
	lost := receiveState(t, states)
	if lost.Mapping != nil || !strings.Contains(lost.Error, "expired") {
		t.Fatalf("lost state = %+v", lost)
	}
	replacement := receiveState(t, states)
	if replacement.Mapping == nil || replacement.Mapping.Provider != "second" {
		t.Fatalf("replacement state = %+v", replacement)
	}
	waitForTestSignal(t, firstStopped, "failed provider shutdown")

	finishMapper(t, cancel, done)
}

func TestGatewayCombinesProviderFailuresAndBacksOff(t *testing.T) {
	clock := newControlledClock()
	var firstRuns atomic.Int32
	var secondRuns atomic.Int32
	failing := func(counter *atomic.Int32, message string) Mapper {
		return mapperFunc(func(ctx context.Context, _ uint16, states chan<- State) error {
			counter.Add(1)
			states <- State{Error: message}
			<-ctx.Done()
			return nil
		})
	}
	mapper := &Gateway{
		providers: []mapperProvider{
			{name: "UPnP", mapper: failing(&firstRuns, "no IGD")},
			{name: providerNATPMP, mapper: failing(&secondRuns, "no default gateway")},
		},
		wait: clock.wait,
		timing: gatewayTiming{
			retryInitial: time.Second,
			retryMaximum: 4 * time.Second,
		},
	}
	cancel, states, done := runGatewayMapper(t, mapper, 44000)

	state := requireFailureState(t, states)
	if !strings.Contains(state.Error, "UPnP: no IGD") || !strings.Contains(state.Error, "NAT-PMP: no default gateway") {
		t.Fatalf("combined error = %q", state.Error)
	}
	if delay := clock.nextWait(t); delay != time.Second {
		t.Fatalf("retry delay = %s", delay)
	}
	if firstRuns.Load() != 1 || secondRuns.Load() != 1 {
		t.Fatalf("provider runs = %d, %d", firstRuns.Load(), secondRuns.Load())
	}

	finishMapper(t, cancel, done)
}

func TestGatewayRejectsInvalidArguments(t *testing.T) {
	mapper := &Gateway{providers: []mapperProvider{{name: "unused", mapper: mapperFunc(func(context.Context, uint16, chan<- State) error {
		return errors.New("must not run")
	})}}}
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
}

func TestUPnPGatewayCandidatesAreUniqueBoundedAndPreferred(t *testing.T) {
	gateways := make([]gateway, 0, maxUPnPGatewayCandidates)
	seen := make(map[string]struct{})
	for index := range 8 {
		gateways = appendUniqueGateway(gateways, seen, &fakeGateway{key: fmt.Sprintf("ip2-%d", index), name: providerWANIP2})
	}
	gateways = appendUniqueGateway(gateways, seen, &fakeGateway{key: "ip2-0", name: providerWANIP2})
	for index := range 9 {
		gateways = appendUniqueGateway(gateways, seen, &fakeGateway{key: fmt.Sprintf("ip1-%d", index), name: providerWANIP1})
	}
	for index := range 4 {
		gateways = appendUniqueGateway(gateways, seen, &fakeGateway{key: fmt.Sprintf("ppp-%d", index), name: providerWANPPP})
	}

	if len(gateways) != maxUPnPGatewayCandidates {
		t.Fatalf("retained gateways = %d, want %d", len(gateways), maxUPnPGatewayCandidates)
	}
	for index, candidate := range gateways {
		want := providerWANIP2
		if index >= 8 {
			want = providerWANIP1
		}
		if candidate.provider() != want {
			t.Fatalf("gateway %d provider = %q, want %q", index, candidate.provider(), want)
		}
	}
}

func waitForTestSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

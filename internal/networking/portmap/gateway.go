package portmap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

type mapperProvider struct {
	name   string
	mapper Mapper
}

type gatewayTiming struct {
	retryInitial time.Duration
	retryMaximum time.Duration
}

type providerRunError struct {
	err        error
	retryAfter time.Time
}

func (err *providerRunError) Error() string { return err.err.Error() }
func (err *providerRunError) Unwrap() error { return err.err }

// Gateway maintains a mapping with the first available gateway protocol and
// fails over when that protocol loses its mapping.
type Gateway struct {
	providers []mapperProvider
	wait      waitFunc
	timing    gatewayTiming
	running   atomic.Bool
}

var _ Mapper = (*Gateway)(nil)

// NewGateway creates an IPv4 mapper that tries PCP, NAT-PMP, and UPnP in order.
func NewGateway(logger *slog.Logger) Mapper {
	if logger == nil {
		logger = slog.Default()
	}
	return newGateway([]mapperProvider{
		{name: providerPCP, mapper: NewPCP(logger)},
		{name: providerNATPMP, mapper: NewNATPMP(logger)},
		{name: "UPnP", mapper: NewUPnP(logger)},
	})
}

// NewIPv6PCP creates an IPv6 PCP mapper. RFC 7723 places the well-known
// anycast address after the default router list, so the existing Gateway
// lifecycle can provide that ordered failover without combining mappings.
func NewIPv6PCP(logger *slog.Logger) Mapper {
	return newGateway([]mapperProvider{
		{name: "PCP IPv6 default gateway", mapper: newPCP(logger, discoverIPv6PCPGateway)},
		{name: "PCP IPv6 anycast", mapper: newPCP(logger, discoverIPv6PCPAnycast)},
	})
}

func newGateway(providers []mapperProvider) Mapper {
	return &Gateway{
		providers: providers,
		wait:      waitContext,
		timing: gatewayTiming{
			retryInitial: defaultRetryInitial,
			retryMaximum: defaultRetryMaximum,
		},
	}
}

// Run maintains one mapping until ctx is canceled. A provider that cannot
// establish a mapping yields to the next provider instead of retaining its own
// retry loop.
func (g *Gateway) Run(ctx context.Context, internalPort uint16, states chan<- State) error {
	if ctx == nil {
		return errors.New("gateway mapper context is required")
	}
	if internalPort == 0 {
		return errors.New("gateway mapper requires a non-zero internal port")
	}
	if states == nil {
		return errors.New("gateway mapper state channel is required")
	}
	if !g.running.CompareAndSwap(false, true) {
		return errors.New("gateway mapper is already running")
	}
	defer g.running.Store(false)
	if len(g.providers) == 0 {
		return errors.New("gateway mapper has no providers")
	}

	timing := normalizeGatewayTiming(g.timing)
	wait := g.wait
	if wait == nil {
		wait = waitContext
	}
	retry := timing.retryInitial
	nextProvider := 0
	providerRetryAfter := make([]time.Time, len(g.providers))
	for {
		var failures errorSummary
		activated := false
		for offset := range len(g.providers) {
			index := (nextProvider + offset) % len(g.providers)
			if time.Now().Before(providerRetryAfter[index]) {
				continue
			}
			provider := g.providers[index]
			name := boundedText(provider.name, maxProviderLength)
			if name == "" {
				name = fmt.Sprintf("provider %d", index+1)
			}
			if provider.mapper == nil {
				failures.addText(name + ": mapper is unavailable")
				continue
			}

			wasActive, err := runMapperProvider(ctx, provider.mapper, name, internalPort, states)
			if ctx.Err() != nil {
				return nil
			}
			if wasActive {
				nextProvider = (index + 1) % len(g.providers)
				retry = timing.retryInitial
				activated = true
				break
			}
			var providerErr *providerRunError
			if errors.As(err, &providerErr) && time.Now().Before(providerErr.retryAfter) {
				providerRetryAfter[index] = providerErr.retryAfter
			}
			failures.add(name, err)
		}
		if activated {
			continue
		}

		failure := failures.err("gateway port mapping unavailable")
		if !publishState(ctx, states, State{Error: failure.Error()}) {
			return nil
		}
		if !wait(ctx, retry) {
			return nil
		}
		retry = nextRetry(retry, timing.retryMaximum)
	}
}

func runMapperProvider(ctx context.Context, mapper Mapper, name string, internalPort uint16, states chan<- State) (bool, error) {
	providerCtx, cancel := context.WithCancel(ctx)
	providerStates := make(chan State, 4)
	result := make(chan error, 1)
	go func() {
		result <- mapper.Run(providerCtx, internalPort, providerStates)
	}()

	active := false
	stop := func() error {
		cancel()
		return <-result
	}
	for {
		select {
		case state := <-providerStates:
			if state.Mapping != nil {
				active = true
				if !publishState(ctx, states, state) {
					_ = stop()
					return true, ctx.Err()
				}
				continue
			}

			if state.Error == "" {
				state.Error = name + " mapping became unavailable"
			}
			stateErr := error(&providerRunError{err: errors.New(boundedText(state.Error, maxErrorLength)), retryAfter: state.RetryAfter})
			if active && !publishState(ctx, states, state) {
				_ = stop()
				return true, ctx.Err()
			}
			runErr := stop()
			if runErr != nil && !errors.Is(runErr, context.Canceled) {
				stateErr = combineErrors("mapper failed", stateErr, runErr)
			}
			return active, stateErr

		case runErr := <-result:
			cancel()
			if ctx.Err() != nil {
				return active, ctx.Err()
			}
			if runErr == nil {
				runErr = errors.New("mapper stopped unexpectedly")
			}
			runErr = fmt.Errorf("%s: %w", name, runErr)
			if active {
				if !publishState(ctx, states, State{Error: boundedError(runErr)}) {
					return true, ctx.Err()
				}
			}
			return active, runErr

		case <-ctx.Done():
			_ = stop()
			return active, ctx.Err()
		}
	}
}

func normalizeGatewayTiming(timing gatewayTiming) gatewayTiming {
	if timing.retryInitial <= 0 {
		timing.retryInitial = defaultRetryInitial
	}
	if timing.retryMaximum < timing.retryInitial {
		timing.retryMaximum = timing.retryInitial
	}
	return timing
}

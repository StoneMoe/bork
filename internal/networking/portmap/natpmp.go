package portmap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	natpmp "github.com/jackpal/go-nat-pmp"
)

const (
	providerNATPMP    = "NAT-PMP"
	natPMPProtocolUDP = "udp"
)

type natPMPExternalResult struct {
	address netip.Addr
	epoch   uint32
}

type natPMPPortResult struct {
	epoch        uint32
	internalPort uint16
	externalPort uint16
	lifetime     uint32
}

type natPMPClient interface {
	externalAddress() (natPMPExternalResult, error)
	addUDPMapping(uint16, uint16, uint32) (natPMPPortResult, error)
}

type natPMPDiscoverFunc func(context.Context) ([]net.IP, error)
type natPMPClientFunc func(net.IP, time.Duration) natPMPClient

// NATPMP is a Mapper backed by the NAT Port Mapping Protocol.
type NATPMP struct {
	logger    *slog.Logger
	discover  natPMPDiscoverFunc
	newClient natPMPClientFunc
	now       func() time.Time
	wait      waitFunc
	timing    mapperTiming
	running   atomic.Bool
}

type natPMPMapping struct {
	gateway      netip.Addr
	internalPort uint16
	externalPort uint16
	leaseSeconds uint32
	advertised   bool
	mapping      Mapping
}

type libraryNATPMPClient struct {
	client *natpmp.Client
}

var _ Mapper = (*NATPMP)(nil)

// NewNATPMP creates a NAT-PMP mapper. Gateway discovery and router I/O begin
// in Run.
func NewNATPMP(logger *slog.Logger) Mapper {
	if logger == nil {
		logger = slog.Default()
	}
	timing := defaultMapperTiming
	timing.operationTimeout = 2 * time.Second
	timing.cleanupTimeout = 2 * time.Second
	return &NATPMP{
		logger:   logger,
		discover: discoverNATPMPGateways,
		newClient: func(gateway net.IP, timeout time.Duration) natPMPClient {
			return &libraryNATPMPClient{client: natpmp.NewClientWithTimeout(gateway, timeout)}
		},
		now:    time.Now,
		wait:   waitContext,
		timing: timing,
	}
}

// Run maintains a finite NAT-PMP UDP mapping until ctx is canceled.
func (n *NATPMP) Run(ctx context.Context, internalPort uint16, states chan<- State) error {
	if ctx == nil {
		return errors.New("NAT-PMP context is required")
	}
	if internalPort == 0 {
		return errors.New("NAT-PMP requires a non-zero internal port")
	}
	if states == nil {
		return errors.New("NAT-PMP state channel is required")
	}
	if !n.running.CompareAndSwap(false, true) {
		return errors.New("NAT-PMP mapper is already running")
	}
	defer n.running.Store(false)

	timing := normalizeTiming(n.timing)
	var owned *natPMPMapping
	defer func() {
		if owned != nil {
			n.cleanup(owned, timing.cleanupTimeout)
		}
	}()

	retry := timing.retryInitial
	lastFailure := ""
	for {
		if ctx.Err() != nil {
			return nil
		}

		if owned != nil && !owned.validAt(n.now()) {
			n.cleanup(owned, timing.cleanupTimeout)
			owned = nil
			if !publishState(ctx, states, State{Error: lastFailure}) {
				return nil
			}
		}

		var attemptErr error
		if owned == nil {
			owned, attemptErr = n.establish(ctx, internalPort, timing)
		} else {
			attemptErr = n.refresh(ctx, owned, timing.operationTimeout)
		}
		if ctx.Err() != nil {
			return nil
		}
		if owned != nil && !owned.validAt(n.now()) {
			n.cleanup(owned, timing.cleanupTimeout)
			owned = nil
			if attemptErr == nil {
				attemptErr = errors.New("NAT-PMP mapping expired before it could be published")
			}
		}

		if attemptErr == nil {
			lastFailure = ""
			retry = timing.retryInitial
			if !publishState(ctx, states, owned.state("", n.now())) {
				return nil
			}
			if !n.wait(ctx, owned.refreshDelay(n.now())) {
				return nil
			}
			continue
		}

		lastFailure = boundedError(attemptErr)
		now := n.now()
		if owned != nil && !owned.validAt(now) {
			n.cleanup(owned, timing.cleanupTimeout)
			owned = nil
		}
		state := State{Error: lastFailure}
		if owned != nil {
			state = owned.state(lastFailure, now)
		}
		if !publishState(ctx, states, state) {
			return nil
		}
		n.logger.Warn("NAT-PMP port mapping unavailable", "error", lastFailure)

		delay := retry
		if owned != nil {
			untilExpiry := owned.mapping.ExpiresAt.Sub(now)
			if untilExpiry < delay {
				delay = untilExpiry
			}
		}
		if !n.wait(ctx, delay) {
			return nil
		}
		retry = nextRetry(retry, timing.retryMaximum)
	}
}

func (n *NATPMP) establish(ctx context.Context, internalPort uint16, timing mapperTiming) (*natPMPMapping, error) {
	discoverCtx, cancel := context.WithTimeout(ctx, timing.discoveryTimeout)
	gateways, discoveryErr := n.discover(discoverCtx)
	cancel()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	var failures errorSummary
	failures.add("discovery", discoveryErr)
	seen := make(map[netip.Addr]struct{}, len(gateways))
	attempted := 0
	for _, rawGateway := range gateways {
		gateway, err := validNATPMPGateway(rawGateway)
		if err != nil {
			failures.add("gateway", err)
			continue
		}
		if _, duplicate := seen[gateway]; duplicate {
			continue
		}
		seen[gateway] = struct{}{}
		attempted++

		created, err := n.create(ctx, gateway, internalPort, leaseSeconds(timing.leaseDuration), timing.operationTimeout)
		if created != nil {
			return created, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		failures.add(gateway.String(), err)
	}
	if attempted == 0 {
		failures.addText("no usable IPv4 default gateway found")
	}
	return nil, failures.err("create NAT-PMP UDP mapping")
}

func (n *NATPMP) create(ctx context.Context, gateway netip.Addr, internalPort uint16, lease uint32, timeout time.Duration) (*natPMPMapping, error) {
	client, err := n.operationClient(ctx, gateway, timeout)
	if err != nil {
		return nil, err
	}
	external, err := queryNATPMPExternalAddress(ctx, client)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	started := n.now()
	owned := newNATPMPMapping(gateway, internalPort, external.address, lease, started)
	result, err := client.addUDPMapping(internalPort, internalPort, lease)
	if err != nil {
		return owned, fmt.Errorf("add NAT-PMP UDP mapping: %w", err)
	}
	if err := applyNATPMPResult(owned, external, result, lease, started); err != nil {
		return owned, err
	}
	if err := ctx.Err(); err != nil {
		return owned, err
	}
	return owned, nil
}

func (n *NATPMP) refresh(ctx context.Context, owned *natPMPMapping, timeout time.Duration) error {
	client, err := n.operationClient(ctx, owned.gateway, timeout)
	if err != nil {
		return err
	}

	// NAT-PMP does not include the WAN address in a mapping response. Query it
	// before every renewal so a changed WAN address is published with the new
	// lease rather than retaining the address from the previous lease.
	external, err := queryNATPMPExternalAddress(ctx, client)
	if err != nil {
		return fmt.Errorf("query public IPv4 before NAT-PMP renewal: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	started := n.now()
	result, err := client.addUDPMapping(owned.internalPort, owned.externalPort, owned.leaseSeconds)
	if err != nil {
		return fmt.Errorf("renew NAT-PMP UDP mapping: %w", err)
	}
	if err := applyNATPMPResult(owned, external, result, owned.leaseSeconds, started); err != nil {
		return fmt.Errorf("validate renewed NAT-PMP UDP mapping: %w", err)
	}
	return ctx.Err()
}

func (n *NATPMP) operationClient(ctx context.Context, gateway netip.Addr, timeout time.Duration) (natPMPClient, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if n.newClient == nil {
		return nil, errors.New("NAT-PMP client factory is unavailable")
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, context.DeadlineExceeded
		}
		if timeout <= 0 || remaining < timeout {
			timeout = remaining
		}
	}
	if timeout <= 0 {
		timeout = operationTimeout
	}
	client := n.newClient(net.IP(gateway.AsSlice()), timeout)
	if client == nil {
		return nil, errors.New("NAT-PMP client is unavailable")
	}
	return client, nil
}

func queryNATPMPExternalAddress(ctx context.Context, client natPMPClient) (natPMPExternalResult, error) {
	if err := ctx.Err(); err != nil {
		return natPMPExternalResult{}, err
	}
	result, err := client.externalAddress()
	if err != nil {
		return natPMPExternalResult{}, fmt.Errorf("query NAT-PMP external address: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return natPMPExternalResult{}, err
	}
	result.address = result.address.Unmap()
	if !publiclyRoutableIPv4(result.address) {
		return natPMPExternalResult{}, errors.New("NAT-PMP external address is not public IPv4")
	}
	return result, nil
}

func applyNATPMPResult(owned *natPMPMapping, external natPMPExternalResult, result natPMPPortResult, requestedLease uint32, started time.Time) error {
	if result.internalPort != owned.internalPort {
		return errors.New("NAT-PMP gateway returned a different internal port")
	}
	if result.externalPort == 0 {
		return errors.New("NAT-PMP gateway returned a zero external port")
	}
	if result.lifetime == 0 {
		return errors.New("NAT-PMP gateway returned a zero mapping lifetime")
	}
	if result.epoch < external.epoch {
		return errors.New("NAT-PMP gateway restarted between address query and mapping")
	}
	lease := conservativeLease(requestedLease, result.lifetime)
	owned.externalPort = result.externalPort
	owned.leaseSeconds = lease
	owned.advertised = true
	owned.mapping.ExternalAddress = netip.AddrPortFrom(external.address, result.externalPort)
	owned.mapping.ExpiresAt = started.Add(time.Duration(lease) * time.Second)
	return nil
}

func newNATPMPMapping(gateway netip.Addr, internalPort uint16, externalAddress netip.Addr, lease uint32, started time.Time) *natPMPMapping {
	return &natPMPMapping{
		gateway:      gateway,
		internalPort: internalPort,
		externalPort: internalPort,
		leaseSeconds: lease,
		mapping: Mapping{
			ExternalAddress: netip.AddrPortFrom(externalAddress, internalPort),
			Provider:        providerNATPMP,
			ExpiresAt:       started.Add(time.Duration(lease) * time.Second),
		},
	}
}

func (n *NATPMP) cleanup(owned *natPMPMapping, timeout time.Duration) {
	if n.newClient == nil || !owned.advertised {
		return
	}
	if timeout <= 0 {
		timeout = cleanupTimeout
	}
	client := n.newClient(net.IP(owned.gateway.AsSlice()), timeout)
	if client == nil {
		n.logger.Warn("failed to remove NAT-PMP UDP mapping", "error", "NAT-PMP client is unavailable")
		return
	}
	if _, err := client.addUDPMapping(owned.internalPort, 0, 0); err != nil {
		n.logger.Warn("failed to remove NAT-PMP UDP mapping", "error", boundedError(err))
	}
}

func (owned *natPMPMapping) validAt(now time.Time) bool {
	return mappingValidAt(owned.mapping, now)
}

func (owned *natPMPMapping) state(message string, now time.Time) State {
	return activeMappingState(owned.mapping, owned.advertised, message, now)
}

func (owned *natPMPMapping) refreshDelay(now time.Time) time.Duration {
	return mappingRefreshDelay(owned.mapping, owned.leaseSeconds, now)
}

func validNATPMPGateway(ip net.IP) (netip.Addr, error) {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.Addr{}, errors.New("NAT-PMP gateway address is invalid")
	}
	address = address.Unmap()
	if !address.Is4() || !address.IsGlobalUnicast() || address.IsLoopback() || address.IsLinkLocalUnicast() {
		return netip.Addr{}, errors.New("NAT-PMP gateway is not usable IPv4")
	}
	return address, nil
}

func discoverNATPMPGateways(ctx context.Context) ([]net.IP, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	route, err := discoverDefaultRoute(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []net.IP{append(net.IP(nil), route.gateway...)}, nil
}

func (client *libraryNATPMPClient) externalAddress() (natPMPExternalResult, error) {
	if client == nil || client.client == nil {
		return natPMPExternalResult{}, errors.New("NAT-PMP client is unavailable")
	}
	result, err := client.client.GetExternalAddress()
	if err != nil {
		return natPMPExternalResult{}, err
	}
	if result == nil {
		return natPMPExternalResult{}, errors.New("NAT-PMP gateway returned no address response")
	}
	return natPMPExternalResult{
		address: netip.AddrFrom4(result.ExternalIPAddress),
		epoch:   result.SecondsSinceStartOfEpoc,
	}, nil
}

func (client *libraryNATPMPClient) addUDPMapping(internalPort, externalPort uint16, lifetime uint32) (natPMPPortResult, error) {
	if client == nil || client.client == nil {
		return natPMPPortResult{}, errors.New("NAT-PMP client is unavailable")
	}
	result, err := client.client.AddPortMapping(natPMPProtocolUDP, int(internalPort), int(externalPort), int(lifetime))
	if err != nil {
		return natPMPPortResult{}, err
	}
	if result == nil {
		return natPMPPortResult{}, errors.New("NAT-PMP gateway returned no mapping response")
	}
	return natPMPPortResult{
		epoch:        result.SecondsSinceStartOfEpoc,
		internalPort: result.InternalPort,
		externalPort: result.MappedExternalPort,
		lifetime:     result.PortMappingLifetimeInSeconds,
	}, nil
}

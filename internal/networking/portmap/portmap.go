// Package portmap manages a UDP port mapping through supported gateway
// protocols.
package portmap

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/huin/goupnp/soap"
)

const (
	mappingDescriptionPrefix = "Bork-"
	mappingTokenBytes        = 16
	mappingProtocol          = "UDP"

	requestedLease      = time.Hour
	defaultRetryInitial = 2 * time.Second
	defaultRetryMaximum = time.Minute
	discoveryTimeout    = 8 * time.Second
	operationTimeout    = 8 * time.Second
	cleanupTimeout      = 3 * time.Second

	fallbackPortAttempts = 4
	highPortBase         = uint16(49152)
	maxErrorLength       = 512
	maxErrorPartLength   = 160
	maxProviderLength    = 96
)

// Mapping describes the externally reachable UDP address created by a Mapper.
// ExpiresAt is a conservative upper bound on the finite router lease.
type Mapping struct {
	ExternalAddress netip.AddrPort
	Provider        string
	ExpiresAt       time.Time
}

// State is a complete snapshot of a mapper's state. Each emitted State
// replaces the preceding one.
type State struct {
	Mapping    *Mapping
	Error      string
	RetryAfter time.Time
}

// Mapper maintains one UDP port mapping until its context is canceled. It
// sends complete snapshots but does not close the caller-owned state channel.
type Mapper interface {
	Run(context.Context, uint16, chan<- State) error
}

type portMappingEntry struct {
	internalPort  uint16
	internalIP    string
	enabled       bool
	description   string
	leaseDuration uint32
}

type gateway interface {
	id() string
	provider() string
	localAddr() net.IP
	addPortMapping(context.Context, uint16, uint16, string, string, uint32) error
	getSpecificPortMapping(context.Context, uint16) (portMappingEntry, error)
	externalIPAddress(context.Context) (string, error)
	deletePortMapping(context.Context, uint16) error
}

type discoverFunc func(context.Context) ([]gateway, error)
type waitFunc func(context.Context, time.Duration) bool

type mapperTiming struct {
	leaseDuration    time.Duration
	retryInitial     time.Duration
	retryMaximum     time.Duration
	discoveryTimeout time.Duration
	operationTimeout time.Duration
	cleanupTimeout   time.Duration
}

var defaultMapperTiming = mapperTiming{
	leaseDuration:    requestedLease,
	retryInitial:     defaultRetryInitial,
	retryMaximum:     defaultRetryMaximum,
	discoveryTimeout: discoveryTimeout,
	operationTimeout: operationTimeout,
	cleanupTimeout:   cleanupTimeout,
}

// UPnP is a Mapper backed by UPnP IGD WAN connection services.
type UPnP struct {
	logger   *slog.Logger
	discover discoverFunc
	random   io.Reader
	now      func() time.Time
	wait     waitFunc
	timing   mapperTiming
	running  atomic.Bool
}

type ownedMapping struct {
	gateway      gateway
	internalPort uint16
	internalIP   string
	externalPort uint16
	description  string
	leaseSeconds uint32
	confirmed    bool
	advertised   bool
	mapping      Mapping
}

type portAttemptOutcome uint8

const (
	portAttemptGatewayFailure portAttemptOutcome = iota
	portAttemptConflict
	portAttemptAmbiguous
)

type ambiguousMappingError struct {
	err error
}

func (err *ambiguousMappingError) Error() string { return err.err.Error() }
func (err *ambiguousMappingError) Unwrap() error { return err.err }

var _ Mapper = (*UPnP)(nil)

// NewUPnP creates a UPnP IGD mapper. Discovery and router I/O begin in Run.
func NewUPnP(logger *slog.Logger) Mapper {
	if logger == nil {
		logger = slog.Default()
	}
	return &UPnP{
		logger:   logger,
		discover: discoverGateways,
		random:   rand.Reader,
		now:      time.Now,
		wait:     waitContext,
		timing:   defaultMapperTiming,
	}
}

// Run maintains a mapping for internalPort and publishes complete state
// snapshots until ctx is canceled.
func (u *UPnP) Run(ctx context.Context, internalPort uint16, states chan<- State) error {
	if ctx == nil {
		return errors.New("UPnP context is required")
	}
	if internalPort == 0 {
		return errors.New("UPnP requires a non-zero internal port")
	}
	if states == nil {
		return errors.New("UPnP state channel is required")
	}
	if !u.running.CompareAndSwap(false, true) {
		return errors.New("UPnP mapper is already running")
	}
	defer u.running.Store(false)

	description, err := randomMappingDescription(u.random)
	if err != nil {
		return errors.New(boundedText("generate UPnP mapping token: "+err.Error(), maxErrorLength))
	}
	timing := normalizeTiming(u.timing)
	var owned *ownedMapping
	defer func() {
		if owned != nil {
			u.cleanup(owned, timing.cleanupTimeout)
		}
	}()

	retry := timing.retryInitial
	lastFailure := ""
	for {
		if ctx.Err() != nil {
			return nil
		}

		if owned != nil && !owned.validAt(u.now()) {
			u.cleanup(owned, timing.cleanupTimeout)
			owned = nil
			if !publishState(ctx, states, State{Error: lastFailure}) {
				return nil
			}
		}

		var attemptErr error
		if owned == nil {
			owned, attemptErr = u.establish(ctx, internalPort, description, timing)
		} else {
			var keep bool
			keep, attemptErr = u.refresh(ctx, owned, timing)
			if !keep {
				owned = nil
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		if owned != nil && !owned.validAt(u.now()) {
			u.cleanup(owned, timing.cleanupTimeout)
			owned = nil
			if attemptErr == nil {
				attemptErr = errors.New("UPnP mapping expired before it could be published")
			}
		}

		if attemptErr == nil {
			lastFailure = ""
			retry = timing.retryInitial
			if !publishState(ctx, states, owned.state("", u.now())) {
				return nil
			}
			if !u.wait(ctx, owned.refreshDelay(u.now())) {
				return nil
			}
			continue
		}

		lastFailure = boundedError(attemptErr)
		now := u.now()
		if owned != nil && !owned.validAt(now) {
			u.cleanup(owned, timing.cleanupTimeout)
			owned = nil
		}
		state := State{Error: lastFailure}
		if owned != nil {
			state = owned.state(lastFailure, now)
		}
		if !publishState(ctx, states, state) {
			return nil
		}
		u.logger.Warn("UPnP port mapping unavailable", "error", lastFailure)

		delay := retry
		if owned != nil {
			untilExpiry := owned.mapping.ExpiresAt.Sub(now)
			if untilExpiry < delay {
				delay = untilExpiry
			}
		}
		if !u.wait(ctx, delay) {
			return nil
		}
		retry = nextRetry(retry, timing.retryMaximum)
	}
}

func (u *UPnP) establish(ctx context.Context, internalPort uint16, description string, timing mapperTiming) (*ownedMapping, error) {
	discoverCtx, cancel := context.WithTimeout(ctx, timing.discoveryTimeout)
	gateways, discoveryErr := u.discover(discoverCtx)
	cancel()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	var failures errorSummary
	failures.add("discovery", discoveryErr)
	seen := make(map[string]struct{}, len(gateways))
	attempted := 0
	for _, candidate := range gateways {
		if candidate == nil {
			continue
		}
		id := candidate.id()
		if id != "" {
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
		}
		attempted++

		created, err := u.create(ctx, candidate, internalPort, description, leaseSeconds(timing.leaseDuration), timing.operationTimeout)
		if created != nil {
			return created, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		failures.add(safeProvider(candidate.provider()), err)
		var ambiguous *ambiguousMappingError
		if errors.As(err, &ambiguous) {
			return nil, failures.err("create UPnP port mapping")
		}
	}
	if attempted == 0 {
		failures.addText("no UPnP IGD WAN connection services found")
	}
	return nil, failures.err("create UPnP port mapping")
}

func (u *UPnP) create(ctx context.Context, candidate gateway, internalPort uint16, description string, lease uint32, timeout time.Duration) (*ownedMapping, error) {
	localAddress, err := validLocalAddress(candidate.localAddr())
	if err != nil {
		return nil, err
	}
	internalIP := localAddress.String()
	used := map[uint16]struct{}{internalPort: {}}
	port := internalPort
	var failures errorSummary

	for attempt := 0; ; attempt++ {
		owned, outcome, err := u.attemptPort(ctx, candidate, port, internalPort, internalIP, description, lease, timeout)
		if owned != nil {
			if !owned.confirmed {
				return owned, &ambiguousMappingError{err: err}
			}
			externalAddress, externalErr := u.externalAddress(ctx, candidate, timeout)
			if externalErr == nil {
				owned.mapping.ExternalAddress = netip.AddrPortFrom(externalAddress, port)
				owned.advertised = true
				return owned, nil
			}

			safeToForget, rollbackErr := u.rollback(ctx, owned, timeout)
			combined := combineErrors("query external IP after creating mapping", externalErr, rollbackErr)
			if safeToForget {
				return nil, combined
			}
			return owned, &ambiguousMappingError{err: combined}
		}
		failures.add(fmt.Sprintf("external port %d", port), err)
		switch outcome {
		case portAttemptAmbiguous:
			return nil, &ambiguousMappingError{err: failures.err("add port mapping")}
		case portAttemptGatewayFailure:
			return nil, failures.err("add port mapping")
		case portAttemptConflict:
			if attempt >= fallbackPortAttempts {
				return nil, failures.err("add port mapping")
			}
		}

		port, err = nextRandomPort(u.random, used)
		if err != nil {
			failures.add("choose fallback port", err)
			return nil, failures.err("add port mapping")
		}
		used[port] = struct{}{}
	}
}

func (u *UPnP) attemptPort(
	ctx context.Context,
	candidate gateway,
	externalPort uint16,
	internalPort uint16,
	internalIP string,
	description string,
	lease uint32,
	timeout time.Duration,
) (*ownedMapping, portAttemptOutcome, error) {
	entry, present, err := u.mappingEntry(ctx, candidate, externalPort, timeout)
	if err != nil {
		return nil, portAttemptGatewayFailure, fmt.Errorf("check existing mapping: %w", err)
	}
	if present && !mappingMatches(entry, internalPort, internalIP, description) {
		return nil, portAttemptConflict, errors.New("external port already has a different UDP mapping")
	}
	if present && entry.leaseDuration == 0 {
		owned := newMappingOwnership(candidate, externalPort, internalPort, internalIP, description)
		return nil, portAttemptGatewayFailure, u.rejectPermanentMapping(ctx, owned, timeout)
	}

	started := u.now()
	addErr := u.addMapping(ctx, candidate, externalPort, internalPort, internalIP, description, lease, timeout)
	verified, verifiedPresent, verifyErr := u.mappingEntry(ctx, candidate, externalPort, timeout)
	if verifyErr != nil {
		if code, isSOAP := upnpErrorCode(addErr); isSOAP {
			if code == 718 {
				return nil, portAttemptConflict, combineErrors("router reported port conflict 718", addErr, verifyErr)
			}
			return nil, portAttemptGatewayFailure, combineErrors("router rejected AddPortMapping", addErr, verifyErr)
		}
		return newPendingMapping(candidate, externalPort, internalPort, internalIP, description, lease, started),
			portAttemptAmbiguous,
			combineErrors("verify added mapping", addErr, verifyErr)
	}
	exact := verifiedPresent && mappingMatches(verified, internalPort, internalIP, description)
	if exact && verified.leaseDuration == 0 {
		owned := newMappingOwnership(candidate, externalPort, internalPort, internalIP, description)
		return nil, portAttemptGatewayFailure, combineErrors(
			"verify temporary UPnP mapping",
			addErr,
			u.rejectPermanentMapping(ctx, owned, timeout),
		)
	}
	if addErr == nil {
		if !exact {
			return newPendingMapping(candidate, externalPort, internalPort, internalIP, description, lease, started),
				portAttemptAmbiguous,
				errors.New("router accepted AddPortMapping but ownership could not be verified")
		}
		return newOwnedMapping(candidate, externalPort, internalPort, internalIP, description, lease, verified.leaseDuration, started), portAttemptGatewayFailure, nil
	}

	code, isSOAP := upnpErrorCode(addErr)
	if !isSOAP && exact {
		return newOwnedMapping(candidate, externalPort, internalPort, internalIP, description, lease, verified.leaseDuration, started), portAttemptGatewayFailure, nil
	}
	if exact {
		return newPendingMapping(candidate, externalPort, internalPort, internalIP, description, lease, started),
			portAttemptAmbiguous,
			fmt.Errorf("router returned SOAP error %d but the run token is present", code)
	}
	if isSOAP && code == 718 {
		return nil, portAttemptConflict, fmt.Errorf("router rejected external port %d with conflict 718", externalPort)
	}
	if isSOAP {
		return nil, portAttemptGatewayFailure, fmt.Errorf("router rejected AddPortMapping with UPnP error %d", code)
	}
	return newPendingMapping(candidate, externalPort, internalPort, internalIP, description, lease, started),
		portAttemptAmbiguous,
		combineErrors("AddPortMapping result is ambiguous", addErr)
}

func (u *UPnP) refresh(ctx context.Context, owned *ownedMapping, timing mapperTiming) (bool, error) {
	entry, present, err := u.mappingEntry(ctx, owned.gateway, owned.externalPort, timing.operationTimeout)
	if err != nil {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		return true, fmt.Errorf("verify mapping before renewal: %w", err)
	}
	if !present || !owned.matches(entry) {
		owned.advertised = false
		return false, errors.New("UPnP mapping ownership was lost before renewal")
	}
	if entry.leaseDuration == 0 {
		owned.advertised = false
		return false, u.rejectPermanentMapping(ctx, owned, timing.operationTimeout)
	}

	started := u.now()
	addErr := u.addMapping(ctx, owned.gateway, owned.externalPort, owned.internalPort, owned.internalIP, owned.description, owned.leaseSeconds, timing.operationTimeout)
	verified, verifiedPresent, verifyErr := u.mappingEntry(ctx, owned.gateway, owned.externalPort, timing.operationTimeout)
	if verifyErr != nil {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		return true, combineErrors("verify renewed mapping", addErr, verifyErr)
	}
	if !verifiedPresent || !owned.matches(verified) {
		owned.advertised = false
		return false, combineErrors("renewed mapping ownership was not verified", addErr)
	}
	if verified.leaseDuration == 0 {
		owned.advertised = false
		return false, combineErrors(
			"verify temporary UPnP mapping renewal",
			addErr,
			u.rejectPermanentMapping(ctx, owned, timing.operationTimeout),
		)
	}

	if addErr != nil {
		return true, fmt.Errorf("mapping renewal returned an error while ownership remained exact: %w", addErr)
	}

	lease := conservativeLease(owned.leaseSeconds, verified.leaseDuration)
	owned.leaseSeconds = lease
	owned.mapping.ExpiresAt = started.Add(time.Duration(lease) * time.Second)
	externalAddress, externalErr := u.externalAddress(ctx, owned.gateway, timing.operationTimeout)
	if externalErr != nil {
		return true, fmt.Errorf("query external IP after renewing mapping: %w", externalErr)
	}

	owned.mapping.ExternalAddress = netip.AddrPortFrom(externalAddress, owned.externalPort)
	owned.advertised = true
	return true, nil
}

func newOwnedMapping(candidate gateway, externalPort, internalPort uint16, internalIP, description string, requested, reportedLease uint32, started time.Time) *ownedMapping {
	lease := conservativeLease(requested, reportedLease)
	return &ownedMapping{
		gateway:      candidate,
		internalPort: internalPort,
		internalIP:   internalIP,
		externalPort: externalPort,
		description:  description,
		leaseSeconds: lease,
		confirmed:    true,
		mapping: Mapping{
			Provider:  safeProvider(candidate.provider()),
			ExpiresAt: started.Add(time.Duration(lease) * time.Second),
		},
	}
}

func newPendingMapping(candidate gateway, externalPort, internalPort uint16, internalIP, description string, lease uint32, started time.Time) *ownedMapping {
	return &ownedMapping{
		gateway:      candidate,
		internalPort: internalPort,
		internalIP:   internalIP,
		externalPort: externalPort,
		description:  description,
		leaseSeconds: lease,
		mapping: Mapping{
			Provider:  safeProvider(candidate.provider()),
			ExpiresAt: started.Add(time.Duration(lease) * time.Second),
		},
	}
}

func newMappingOwnership(candidate gateway, externalPort, internalPort uint16, internalIP, description string) *ownedMapping {
	return &ownedMapping{
		gateway:      candidate,
		internalPort: internalPort,
		internalIP:   internalIP,
		externalPort: externalPort,
		description:  description,
	}
}

func (u *UPnP) mappingEntry(ctx context.Context, candidate gateway, externalPort uint16, timeout time.Duration) (portMappingEntry, bool, error) {
	operationCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return lookupMapping(operationCtx, candidate, externalPort)
}

func lookupMapping(ctx context.Context, candidate gateway, externalPort uint16) (portMappingEntry, bool, error) {
	entry, err := candidate.getSpecificPortMapping(ctx, externalPort)
	if isUPnPError(err, 714) {
		return portMappingEntry{}, false, nil
	}
	if err != nil {
		return portMappingEntry{}, false, err
	}
	return entry, true, nil
}

func (u *UPnP) addMapping(ctx context.Context, candidate gateway, externalPort, internalPort uint16, internalIP, description string, lease uint32, timeout time.Duration) error {
	operationCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return candidate.addPortMapping(operationCtx, externalPort, internalPort, internalIP, description, lease)
}

func (u *UPnP) externalAddress(ctx context.Context, candidate gateway, timeout time.Duration) (netip.Addr, error) {
	operationCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return queryExternalAddress(operationCtx, candidate)
}

func (u *UPnP) rollback(ctx context.Context, owned *ownedMapping, timeout time.Duration) (bool, error) {
	rollbackCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return deleteOwnedMapping(rollbackCtx, owned)
}

func (u *UPnP) rejectPermanentMapping(ctx context.Context, owned *ownedMapping, timeout time.Duration) error {
	leaseErr := errors.New("router reported lease duration 0 for a token-matched mapping; permanent UPnP mappings are not accepted")
	if _, err := u.rollback(ctx, owned, timeout); err != nil {
		return combineErrors("reject permanent UPnP mapping", leaseErr, err)
	}
	return leaseErr
}

func (u *UPnP) cleanup(owned *ownedMapping, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, err := deleteOwnedMapping(ctx, owned); err != nil {
		u.logger.Warn("failed to remove UPnP port mapping", "error", boundedError(err))
	}
}

func deleteOwnedMapping(ctx context.Context, owned *ownedMapping) (bool, error) {
	entry, present, err := lookupMapping(ctx, owned.gateway, owned.externalPort)
	if err != nil {
		return false, fmt.Errorf("verify mapping before delete: %w", err)
	}
	if !present || !owned.matches(entry) {
		return true, nil
	}
	if err := owned.gateway.deletePortMapping(ctx, owned.externalPort); err != nil {
		if isUPnPError(err, 714) {
			return true, nil
		}
		return false, fmt.Errorf("delete verified mapping: %w", err)
	}
	return true, nil
}

func (owned *ownedMapping) matches(entry portMappingEntry) bool {
	return mappingMatches(entry, owned.internalPort, owned.internalIP, owned.description)
}

func mappingMatches(entry portMappingEntry, internalPort uint16, internalIP, description string) bool {
	return entry.internalPort == internalPort &&
		entry.internalIP == internalIP &&
		entry.enabled &&
		entry.description == description
}

func (owned *ownedMapping) validAt(now time.Time) bool {
	return mappingValidAt(owned.mapping, now)
}

func (owned *ownedMapping) state(message string, now time.Time) State {
	return activeMappingState(owned.mapping, owned.advertised, message, now)
}

func (owned *ownedMapping) refreshDelay(now time.Time) time.Duration {
	return mappingRefreshDelay(owned.mapping, owned.leaseSeconds, now)
}

func mappingValidAt(mapping Mapping, now time.Time) bool {
	return now.Before(mapping.ExpiresAt)
}

func activeMappingState(mapping Mapping, advertised bool, message string, now time.Time) State {
	state := State{Error: message}
	if advertised && mappingValidAt(mapping, now) {
		state.Mapping = copyMapping(&mapping)
	}
	return state
}

func mappingRefreshDelay(mapping Mapping, leaseSeconds uint32, now time.Time) time.Duration {
	lease := time.Duration(leaseSeconds) * time.Second
	renewAt := mapping.ExpiresAt.Add(-lease * 3 / 5)
	if delay := renewAt.Sub(now); delay > 0 {
		return delay
	}
	return time.Millisecond
}

func validLocalAddress(ip net.IP) (netip.Addr, error) {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.Addr{}, errors.New("router has no usable local address")
	}
	address = address.Unmap()
	if !address.Is4() || !address.IsGlobalUnicast() {
		return netip.Addr{}, errors.New("router local address is not usable IPv4")
	}
	return address, nil
}

func queryExternalAddress(ctx context.Context, candidate gateway) (netip.Addr, error) {
	raw, err := candidate.externalIPAddress(ctx)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("query external IP address: %w", err)
	}
	address, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return netip.Addr{}, errors.New("router returned an invalid external IP address")
	}
	address = address.Unmap()
	if !publiclyRoutableIPv4(address) {
		return netip.Addr{}, errors.New("router external IP address is not usable IPv4")
	}
	return address, nil
}

var nonPublicIPv4Prefixes = [...]netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

func publiclyRoutableIPv4(address netip.Addr) bool {
	address = address.Unmap()
	if !address.Is4() || !address.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range nonPublicIPv4Prefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func randomMappingDescription(random io.Reader) (string, error) {
	if random == nil {
		return "", errors.New("random source is unavailable")
	}
	var token [mappingTokenBytes]byte
	if _, err := io.ReadFull(random, token[:]); err != nil {
		return "", err
	}
	return mappingDescriptionPrefix + hex.EncodeToString(token[:]), nil
}

func nextRandomPort(random io.Reader, used map[uint16]struct{}) (uint16, error) {
	for range fallbackPortAttempts {
		port, err := randomHighPort(random)
		if err != nil {
			return 0, err
		}
		if _, duplicate := used[port]; !duplicate {
			return port, nil
		}
	}
	return 0, errors.New("random source returned duplicate fallback ports")
}

func randomHighPort(random io.Reader) (uint16, error) {
	if random == nil {
		return 0, errors.New("random source is unavailable")
	}
	var bytes [2]byte
	if _, err := io.ReadFull(random, bytes[:]); err != nil {
		return 0, err
	}
	// The dynamic range has 2^14 ports, so masking is unbiased.
	return highPortBase + (binary.BigEndian.Uint16(bytes[:]) & 0x3fff), nil
}

func upnpErrorCode(err error) (int, bool) {
	var fault *soap.SOAPFaultError
	if !errors.As(err, &fault) {
		return 0, false
	}
	return fault.Detail.UPnPError.Errorcode, true
}

func isUPnPError(err error, code int) bool {
	actual, ok := upnpErrorCode(err)
	return ok && actual == code
}

func conservativeLease(requested, reported uint32) uint32 {
	if reported != 0 && reported < requested {
		return reported
	}
	return requested
}

func leaseSeconds(duration time.Duration) uint32 {
	seconds := duration / time.Second
	if seconds > time.Duration(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(seconds)
}

func normalizeTiming(timing mapperTiming) mapperTiming {
	if timing.leaseDuration < time.Second {
		timing.leaseDuration = requestedLease
	}
	if timing.retryInitial <= 0 {
		timing.retryInitial = defaultRetryInitial
	}
	if timing.retryMaximum < timing.retryInitial {
		timing.retryMaximum = timing.retryInitial
	}
	if timing.discoveryTimeout <= 0 {
		timing.discoveryTimeout = discoveryTimeout
	}
	if timing.operationTimeout <= 0 {
		timing.operationTimeout = operationTimeout
	}
	if timing.cleanupTimeout <= 0 {
		timing.cleanupTimeout = cleanupTimeout
	}
	return timing
}

func nextRetry(current, maximum time.Duration) time.Duration {
	if current >= maximum/2 {
		return maximum
	}
	return current * 2
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	if duration <= 0 {
		duration = time.Millisecond
	}
	select {
	case <-time.After(duration):
		return true
	case <-ctx.Done():
		return false
	}
}

func publishState(ctx context.Context, states chan<- State, state State) bool {
	state.Error = boundedText(state.Error, maxErrorLength)
	select {
	case states <- state:
		return true
	case <-ctx.Done():
		return false
	}
}

func safeProvider(provider string) string {
	provider = boundedText(provider, maxProviderLength)
	if provider == "" {
		return "UPnP IGD"
	}
	return provider
}

func boundedError(err error) string {
	if err == nil {
		return ""
	}
	return boundedText(err.Error(), maxErrorLength)
}

func boundedText(value string, maximum int) string {
	if value == "" || maximum <= 0 {
		return ""
	}
	if len(value) > maximum*4 {
		value = value[:maximum*4]
	}
	value = strings.ToValidUTF8(value, "?")
	value = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, value)
	if len(value) <= maximum {
		return value
	}
	if maximum <= 3 {
		return strings.Repeat(".", maximum)
	}
	end := maximum - 3
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + "..."
}

func combineErrors(prefix string, errs ...error) error {
	var summary errorSummary
	for _, err := range errs {
		summary.add("", err)
	}
	return summary.err(prefix)
}

type errorSummary struct {
	parts   []string
	omitted int
}

func (summary *errorSummary) add(label string, err error) {
	if err == nil {
		return
	}
	text := boundedText(err.Error(), maxErrorPartLength)
	if label != "" {
		text = boundedText(label+": "+text, maxErrorPartLength)
	}
	summary.addText(text)
}

func (summary *errorSummary) addText(text string) {
	if text == "" {
		return
	}
	if len(summary.parts) >= 8 {
		summary.omitted++
		return
	}
	summary.parts = append(summary.parts, boundedText(text, maxErrorPartLength))
}

func (summary *errorSummary) err(prefix string) error {
	parts := append([]string(nil), summary.parts...)
	if summary.omitted != 0 {
		parts = append(parts, fmt.Sprintf("%d additional failures", summary.omitted))
	}
	if len(parts) == 0 {
		parts = append(parts, "operation failed")
	}
	return errors.New(boundedText(prefix+": "+strings.Join(parts, "; "), maxErrorLength))
}

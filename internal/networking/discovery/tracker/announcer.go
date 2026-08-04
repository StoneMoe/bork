package tracker

import (
	"context"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"bork/internal/networking/discovery"
)

const (
	maxProviderURLLength      = 2048
	maxResolvedAddresses      = 8
	maxResolvedAddressesByIP  = 4
	maximumConnectionLifetime = 60 * time.Second
	trackerStopTimeout        = 3 * time.Second
	maxTrackerErrorLength     = 512
)

// Transport exchanges a BEP 15 request through the application's shared UDP
// socket. expectedAction and transaction identify the response to accept.
type Transport interface {
	ExchangeTracker(ctx context.Context, server netip.AddrPort, request []byte, expectedAction, transaction uint32) ([]byte, error)
}

type provider struct {
	display     string
	scope       string
	scheme      string
	host        string
	port        uint16
	announceURL url.URL
}

type announcerTiming struct {
	requestTimeout     time.Duration
	requestAttempts    int
	connectionLifetime time.Duration
	resolveTimeout     time.Duration
	httpRequestTimeout time.Duration
	providerRetry      time.Duration
	providerRetryMax   time.Duration
	intervalMin        time.Duration
	intervalMax        time.Duration
	initialInterval    time.Duration
	initialAnnounces   int
	hintLifetimeMin    time.Duration
	hintLifetimeMax    time.Duration
}

var defaultAnnouncerTiming = announcerTiming{
	requestTimeout:     15 * time.Second,
	requestAttempts:    4,
	connectionLifetime: maximumConnectionLifetime,
	resolveTimeout:     10 * time.Second,
	httpRequestTimeout: 20 * time.Second,
	providerRetry:      5 * time.Second,
	providerRetryMax:   5 * time.Minute,
	intervalMin:        5 * time.Second,
	intervalMax:        30 * time.Second,
	initialInterval:    5 * time.Second,
	initialAnnounces:   3,
	hintLifetimeMin:    time.Minute,
	hintLifetimeMax:    time.Hour,
}

// Announcer discovers peers from independently supervised UDP and HTTP(S)
// trackers. Registration identities are derived per room, provider, and candidate.
type Announcer struct {
	providers   []provider
	infoHash    [20]byte
	identityKey [32]byte
	transport   Transport
	httpClient  *http.Client
	logger      *slog.Logger

	lookupNetIP func(context.Context, string, string) ([]netip.Addr, error)
	random      io.Reader
	timing      announcerTiming
	waitRetry   func(context.Context, time.Duration) bool

	randomMu      sync.Mutex
	candidate     AnnounceCandidate
	running       atomic.Bool
	statusMu      sync.RWMutex
	statuses      map[string]ProviderStatus
	statusChanges chan struct{}
}

type ProviderStatus struct {
	Provider        string `json:"provider"`
	Candidate       string `json:"candidate"`
	ObservedAddress string `json:"observedAddress,omitempty"`
	NextAnnounce    string `json:"nextAnnounce,omitempty"`
	PeerCount       int    `json:"peerCount"`
	Error           string `json:"error,omitempty"`
}

type AnnounceCandidate struct {
	Address netip.Addr
	Port    uint16
}

type trackerRegistration struct {
	candidate AnnounceCandidate
	peerID    [20]byte
	key       uint32
}

func newAnnouncer(providerURLs []string, infoHash [20]byte, identityKey [32]byte, candidate AnnounceCandidate, transport Transport, logger *slog.Logger) (*Announcer, error) {
	providers, err := parseProviders(providerURLs)
	if err != nil {
		return nil, err
	}
	return newAnnouncerFromProviders(providers, infoHash, identityKey, candidate, transport, logger)
}

func ValidateProviderURL(raw string) error {
	_, err := parseProvider(raw)
	return err
}

func newAnnouncerFromProviders(providers []provider, infoHash [20]byte, identityKey [32]byte, candidate AnnounceCandidate, transport Transport, logger *slog.Logger) (*Announcer, error) {
	if err := validateProviderConfig(providers, identityKey, transport); err != nil {
		return nil, err
	}
	if normalized, valid := normalizeAnnounceCandidate(candidate); valid {
		candidate = normalized
	} else if candidate != (AnnounceCandidate{}) {
		return nil, errors.New("tracker announce candidate is invalid")
	}

	if logger == nil {
		logger = slog.Default()
	}
	return &Announcer{
		providers:   providers,
		infoHash:    infoHash,
		identityKey: identityKey,
		transport:   transport,
		httpClient:  newHTTPClient(),
		logger:      logger,
		lookupNetIP: func(ctx context.Context, network, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, network, host)
		},
		random:        rand.Reader,
		timing:        defaultAnnouncerTiming,
		waitRetry:     waitForProviderRetry,
		candidate:     candidate,
		statuses:      make(map[string]ProviderStatus, len(providers)),
		statusChanges: make(chan struct{}, 1),
	}, nil
}

func parseProviders(providerURLs []string) ([]provider, error) {
	providers := make([]provider, len(providerURLs))
	for index, raw := range providerURLs {
		parsed, err := parseProvider(raw)
		if err != nil {
			return nil, fmt.Errorf("tracker provider %d: %w", index+1, err)
		}
		providers[index] = parsed
	}
	return providers, nil
}

func validateProviderConfig(providers []provider, identityKey [32]byte, transport Transport) error {
	if len(providers) > 0 && identityKey == [32]byte{} {
		return errors.New("tracker identity key is required")
	}
	for _, configured := range providers {
		if configured.scheme == "udp" && transport == nil {
			return errors.New("UDP tracker transport is required")
		}
	}
	return nil
}

func (a *Announcer) Snapshot() []ProviderStatus {
	a.statusMu.RLock()
	defer a.statusMu.RUnlock()
	statuses := make([]ProviderStatus, 0, len(a.providers))
	for _, configured := range a.providers {
		status := a.statuses[configured.scope]
		status.Provider = configured.display
		status.Candidate = a.candidate.String()
		statuses = append(statuses, status)
	}
	return statuses
}

func (a *Announcer) StatusChanges() <-chan struct{} { return a.statusChanges }

func (candidate AnnounceCandidate) String() string {
	if candidate.Port == 0 {
		return ""
	}
	if candidate.Address.IsValid() {
		return netip.AddrPortFrom(candidate.Address.Unmap(), candidate.Port).String()
	}
	return netip.AddrPortFrom(netip.IPv4Unspecified(), candidate.Port).String()
}

func normalizeAnnounceCandidate(candidate AnnounceCandidate) (AnnounceCandidate, bool) {
	if candidate.Port == 0 {
		return AnnounceCandidate{}, false
	}
	if !candidate.Address.IsValid() {
		candidate.Address = netip.Addr{}
		return candidate, true
	}
	candidate.Address = candidate.Address.Unmap()
	if !candidate.Address.Is4() || !usableTrackerAddress(candidate.Address) || candidate.Address.IsPrivate() || candidate.Address.IsLoopback() || candidate.Address.IsLinkLocalUnicast() {
		return AnnounceCandidate{}, false
	}
	return candidate, true
}

func (a *Announcer) registration(configured provider, candidate AnnounceCandidate) trackerRegistration {
	info := "bork/tracker-registration/v1\x00" + configured.scope + "\x00" + candidate.String()
	material, err := hkdf.Key(sha256.New, a.identityKey[:], a.infoHash[:], info, 24)
	if err != nil {
		panic("derive tracker registration: " + err.Error())
	}
	var registration trackerRegistration
	registration.candidate = candidate
	copy(registration.peerID[:], material[:20])
	registration.key = binary.BigEndian.Uint32(material[20:24])
	if registration.key == 0 {
		registration.key = 1
	}
	return registration
}

// Run announces to every configured provider until ctx is canceled. Provider
// failures are isolated and retried rather than terminating the other workers.
func (a *Announcer) Run(ctx context.Context, hints chan<- discovery.Hint) error {
	if ctx == nil {
		return errors.New("tracker context is required")
	}
	if !a.running.CompareAndSwap(false, true) {
		return errors.New("tracker announcer is already running")
	}
	defer a.running.Store(false)

	if len(a.providers) == 0 {
		<-ctx.Done()
		return nil
	}
	if a.candidate.Port == 0 {
		return errors.New("tracker announce candidate is required")
	}

	timing := normalizeTiming(a.timing)
	var workers sync.WaitGroup
	workers.Add(len(a.providers))
	for _, configured := range a.providers {
		configured := configured
		go func() {
			defer workers.Done()
			a.runProvider(ctx, configured, hints, timing)
		}()
	}
	<-ctx.Done()
	workers.Wait()
	return nil
}

func (a *Announcer) runProvider(ctx context.Context, configured provider, hints chan<- discovery.Hint, timing announcerTiming) {
	retry := timing.providerRetry
	for {
		var announced bool
		var err error
		if configured.scheme == "udp" {
			announced, err = a.runUDPProvider(ctx, configured, hints, timing)
		} else {
			announced, err = a.runHTTPTracker(ctx, configured, hints, timing)
		}
		if ctx.Err() != nil {
			return
		}
		if announced {
			retry = timing.providerRetry
		}
		a.logger.Warn("tracker provider unavailable", "provider", configured.display, "error", err)
		a.recordFailure(configured, a.candidate, err, time.Now().Add(retry))

		if !a.waitRetry(ctx, retry) {
			return
		}
		if retry >= timing.providerRetryMax/2 {
			retry = timing.providerRetryMax
		} else {
			retry *= 2
		}
	}
}

func (a *Announcer) runUDPProvider(ctx context.Context, configured provider, hints chan<- discovery.Hint, timing announcerTiming) (bool, error) {
	addresses, err := a.resolveProvider(ctx, configured, timing.resolveTimeout)
	if err != nil {
		return false, fmt.Errorf("resolve UDP tracker: %w", err)
	}
	announced := false
	var failures []error
	for _, address := range addresses {
		var accepted bool
		accepted, err = a.runTracker(ctx, configured, address, hints, timing)
		announced = announced || accepted
		if ctx.Err() != nil {
			return announced, ctx.Err()
		}
		failures = append(failures, fmt.Errorf("%s: %w", address, err))
	}
	return announced, errors.Join(failures...)
}

func (a *Announcer) runTracker(ctx context.Context, configured provider, address netip.AddrPort, hints chan<- discovery.Hint, timing announcerTiming) (bool, error) {
	candidate := a.candidate
	registration := a.registration(configured, candidate)
	var connectionID uint64
	var connectionExpires time.Time
	started := false
	announces := 0
	announced := false
	var active *trackerRegistration
	defer func() {
		if active != nil {
			a.stopUDPRegistration(configured, address, connectionID, connectionExpires, *active, timing)
		}
	}()

	for {
		if ctx.Err() != nil {
			return announced, ctx.Err()
		}
		if !time.Now().Before(connectionExpires) {
			connected, err := a.connect(ctx, address, timing)
			if err != nil {
				return announced, fmt.Errorf("connect to %s: %w", configured.display, err)
			}
			connectionID = connected
			connectionExpires = time.Now().Add(timing.connectionLifetime)
		}

		event := eventNone
		if !started {
			event = eventStarted
			active = &registration
		}
		announceCtx, cancel := context.WithDeadline(ctx, connectionExpires)
		response, err := a.announce(announceCtx, address, connectionID, event, registration, timing)
		cancel()
		if err != nil {
			if ctx.Err() == nil && !time.Now().Before(connectionExpires) {
				connectionExpires = time.Time{}
				continue
			}
			return announced, fmt.Errorf("announce to %s: %w", configured.display, err)
		}
		started = true
		active = &registration
		interval := effectiveAnnounceInterval(response.interval, timing, announces)
		announces++
		a.recordSuccess(configured, candidate, response, interval)
		announced = true
		if err := publishAndWait(ctx, hints, response, interval, timing); err != nil {
			return announced, err
		}
	}
}

func (a *Announcer) stopUDPRegistration(
	configured provider,
	address netip.AddrPort,
	connectionID uint64,
	connectionExpires time.Time,
	registration trackerRegistration,
	timing announcerTiming,
) {
	ctx, cancel := context.WithTimeout(context.Background(), trackerStopTimeout)
	defer cancel()
	if connectionID == 0 || !time.Now().Before(connectionExpires) {
		connected, err := a.connect(ctx, address, timing)
		if err != nil {
			a.logger.Debug("connect to stop UDP tracker registration", "provider", configured.display, "candidate", registration.candidate.String(), "error", err)
			return
		}
		connectionID = connected
	}
	if _, err := a.announce(ctx, address, connectionID, eventStopped, registration, timing); err != nil {
		a.logger.Debug("stop UDP tracker registration", "provider", configured.display, "candidate", registration.candidate.String(), "error", err)
	}
}

func publishAndWait(ctx context.Context, hints chan<- discovery.Hint, response announceResponse, interval time.Duration, timing announcerTiming) error {
	lifetime := clampDuration(interval*2, timing.hintLifetimeMin, timing.hintLifetimeMax)
	expiresAt := time.Now().Add(lifetime)
	for _, peer := range response.peers {
		select {
		case hints <- discovery.Hint{Address: peer, Source: discovery.SourceTracker, ExpiresAt: expiresAt}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	select {
	case <-time.After(interval):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func effectiveAnnounceInterval(providerInterval time.Duration, timing announcerTiming, announces int) time.Duration {
	interval := clampDuration(providerInterval, timing.intervalMin, timing.intervalMax)
	if announces < timing.initialAnnounces && timing.initialInterval > 0 && timing.initialInterval < interval {
		return timing.initialInterval
	}
	return interval
}

func (a *Announcer) connect(ctx context.Context, address netip.AddrPort, timing announcerTiming) (uint64, error) {
	transaction, err := a.newTransaction()
	if err != nil {
		return 0, err
	}
	response, err := a.exchange(ctx, address, marshalConnectRequest(transaction), actionConnect, transaction, timing)
	if err != nil {
		return 0, err
	}
	return parseConnectResponse(response, transaction)
}

func (a *Announcer) announce(
	ctx context.Context,
	address netip.AddrPort,
	connectionID uint64,
	event uint32,
	registration trackerRegistration,
	timing announcerTiming,
) (announceResponse, error) {
	transaction, err := a.newTransaction()
	if err != nil {
		return announceResponse{}, err
	}
	request := marshalAnnounceRequest(announceRequest{
		connectionID: connectionID,
		transaction:  transaction,
		infoHash:     a.infoHash,
		peerID:       registration.peerID,
		event:        event,
		key:          registration.key,
		numWant:      maxAnnouncePeers,
		port:         registration.candidate.Port,
		explicitIP:   registration.candidate.Address,
	})
	response, err := a.exchange(ctx, address, request, actionAnnounce, transaction, timing)
	if err != nil {
		return announceResponse{}, err
	}
	return parseAnnounceResponse(response, transaction, !address.Addr().Unmap().Is4())
}

func (a *Announcer) exchange(
	ctx context.Context,
	address netip.AddrPort,
	request []byte,
	action uint32,
	transaction uint32,
	timing announcerTiming,
) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < timing.requestAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, exponentialTimeout(timing.requestTimeout, attempt))
		response, err := a.transport.ExchangeTracker(attemptCtx, address, request, action, transaction)
		cancel()
		if err == nil {
			err = validateResponseHeader(response, action, transaction)
			if err == nil {
				return response, nil
			}
			var trackerErr *TrackerError
			if errors.As(err, &trackerErr) {
				return nil, err
			}
		}
		lastErr = err
	}
	return nil, fmt.Errorf("tracker action %d exchange failed after %d attempts: %w", action, timing.requestAttempts, lastErr)
}

func (a *Announcer) resolveProvider(ctx context.Context, configured provider, timeout time.Duration) ([]netip.AddrPort, error) {
	if literal, err := netip.ParseAddr(configured.host); err == nil {
		literal = literal.Unmap()
		if !usableTrackerAddress(literal) {
			return nil, errors.New("tracker address is not usable")
		}
		return []netip.AddrPort{netip.AddrPortFrom(literal, configured.port)}, nil
	}

	resolveCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	addresses, err := a.lookupNetIP(resolveCtx, "ip", configured.host)
	if err != nil {
		return nil, err
	}
	resolved := make([]netip.AddrPort, 0, min(len(addresses), maxResolvedAddresses))
	seen := make(map[netip.Addr]struct{}, maxResolvedAddresses)
	ipv4Count, ipv6Count := 0, 0
	for _, address := range addresses {
		address = address.Unmap()
		if !usableTrackerAddress(address) {
			continue
		}
		if _, exists := seen[address]; exists {
			continue
		}
		if address.Is4() {
			if ipv4Count >= maxResolvedAddressesByIP {
				continue
			}
			ipv4Count++
		} else {
			if ipv6Count >= maxResolvedAddressesByIP {
				continue
			}
			ipv6Count++
		}
		seen[address] = struct{}{}
		resolved = append(resolved, netip.AddrPortFrom(address, configured.port))
		if len(resolved) == maxResolvedAddresses {
			break
		}
	}
	if len(resolved) == 0 {
		return nil, errors.New("tracker hostname resolved to no usable addresses")
	}
	return resolved, nil
}

func (a *Announcer) newTransaction() (uint32, error) {
	a.randomMu.Lock()
	defer a.randomMu.Unlock()
	transaction, err := readNonzeroUint32(a.random)
	if err != nil {
		return 0, fmt.Errorf("generate tracker transaction: %w", err)
	}
	return transaction, nil
}

func parseProvider(raw string) (provider, error) {
	if raw == "" || raw != strings.TrimSpace(raw) || len(raw) > maxProviderURLLength {
		return provider{}, errors.New("tracker URL is invalid")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || parsed.Host == "" {
		return provider{}, errors.New("tracker URL must use udp://, http://, or https://")
	}
	if parsed.Scheme != "udp" && parsed.Scheme != "http" && parsed.Scheme != "https" {
		return provider{}, errors.New("tracker URL must use udp://, http://, or https://")
	}
	if parsed.User != nil || parsed.Fragment != "" || strings.Contains(raw, "#") {
		return provider{}, errors.New("tracker URL contains unsupported credentials or fragment")
	}
	host := parsed.Hostname()
	if host == "" {
		return provider{}, errors.New("tracker URL requires a host")
	}
	portText := parsed.Port()
	if parsed.Scheme == "udp" {
		if parsed.RawQuery != "" {
			return provider{}, errors.New("UDP tracker URL query is unsupported")
		}
		if portText == "" {
			return provider{}, errors.New("UDP tracker URL requires a port")
		}
		port, err := strconv.ParseUint(portText, 10, 16)
		if err != nil || port == 0 {
			return provider{}, errors.New("UDP tracker URL port is invalid")
		}
		scopeURL := *parsed
		return provider{display: raw, scope: scopeURL.String(), scheme: parsed.Scheme, host: host, port: uint16(port)}, nil
	}
	if portText != "" {
		port, err := strconv.ParseUint(portText, 10, 16)
		if err != nil || port == 0 {
			return provider{}, errors.New("HTTP tracker URL port is invalid")
		}
	}
	if err := validateHTTPProviderQuery(parsed.RawQuery); err != nil {
		return provider{}, err
	}
	displayURL := *parsed
	displayURL.RawQuery = ""
	displayURL.ForceQuery = false
	scopeURL := *parsed
	if query, err := url.ParseQuery(scopeURL.RawQuery); err == nil {
		scopeURL.RawQuery = query.Encode()
	}
	return provider{display: displayURL.String(), scope: scopeURL.String(), scheme: parsed.Scheme, host: host, announceURL: *parsed}, nil
}

func readNonzeroUint32(source io.Reader) (uint32, error) {
	var bytes [4]byte
	for range 32 {
		if _, err := io.ReadFull(source, bytes[:]); err != nil {
			return 0, err
		}
		if value := binary.BigEndian.Uint32(bytes[:]); value != 0 {
			return value, nil
		}
	}
	return 0, errors.New("random source produced only zero values")
}

func (a *Announcer) recordSuccess(configured provider, candidate AnnounceCandidate, response announceResponse, interval time.Duration) {
	now := time.Now().UTC()
	observed := observedRegistrationAddress(response, candidate)
	a.recordStatus(configured, ProviderStatus{
		Provider: configured.display, Candidate: candidate.String(),
		NextAnnounce: now.Add(interval).Format(time.RFC3339), PeerCount: len(response.peers), ObservedAddress: observed,
	})
}

func observedRegistrationAddress(response announceResponse, candidate AnnounceCandidate) string {
	if response.externalAddress.IsValid() && candidate.Port != 0 {
		return netip.AddrPortFrom(response.externalAddress.Unmap(), candidate.Port).String()
	}
	if candidate.Address.IsValid() {
		expected := netip.AddrPortFrom(candidate.Address.Unmap(), candidate.Port)
		for _, peer := range response.peers {
			if peer == expected {
				return peer.String()
			}
		}
	}
	var matched netip.AddrPort
	for _, peer := range response.peers {
		if peer.Port() != candidate.Port {
			continue
		}
		if matched.IsValid() && matched != peer {
			return ""
		}
		matched = peer
	}
	if matched.IsValid() {
		return matched.String()
	}
	return ""
}

func (a *Announcer) recordFailure(configured provider, candidate AnnounceCandidate, err error, retryAt time.Time) {
	message := "tracker provider stopped unexpectedly"
	if err != nil {
		message = err.Error()
	}
	message = boundedTrackerText(message, maxTrackerErrorLength)
	a.recordStatus(configured, ProviderStatus{
		Provider: configured.display, Candidate: candidate.String(),
		NextAnnounce: retryAt.UTC().Format(time.RFC3339), Error: message,
	})
}

func boundedTrackerText(value string, maximum int) string {
	if value == "" || maximum <= 0 {
		return ""
	}
	if len(value) > maximum*4 {
		value = value[:maximum*4]
	}
	value = strings.ToValidUTF8(value, "?")
	if len(value) <= maximum {
		return value
	}
	end := maximum
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func (a *Announcer) recordStatus(configured provider, status ProviderStatus) {
	a.statusMu.Lock()
	a.statuses[configured.scope] = status
	a.statusMu.Unlock()
	select {
	case a.statusChanges <- struct{}{}:
	default:
	}
}

func normalizeTiming(timing announcerTiming) announcerTiming {
	defaults := defaultAnnouncerTiming
	if timing.requestTimeout <= 0 {
		timing.requestTimeout = defaults.requestTimeout
	}
	if timing.requestAttempts < 1 {
		timing.requestAttempts = defaults.requestAttempts
	} else if timing.requestAttempts > 8 {
		timing.requestAttempts = 8
	}
	if timing.connectionLifetime <= 0 {
		timing.connectionLifetime = defaults.connectionLifetime
	} else if timing.connectionLifetime > maximumConnectionLifetime {
		timing.connectionLifetime = maximumConnectionLifetime
	}
	if timing.resolveTimeout <= 0 {
		timing.resolveTimeout = defaults.resolveTimeout
	}
	if timing.httpRequestTimeout <= 0 {
		timing.httpRequestTimeout = defaults.httpRequestTimeout
	}
	if timing.providerRetry <= 0 {
		timing.providerRetry = defaults.providerRetry
	}
	if timing.providerRetryMax < timing.providerRetry {
		timing.providerRetryMax = max(defaults.providerRetryMax, timing.providerRetry)
	}
	if timing.intervalMin <= 0 {
		timing.intervalMin = defaults.intervalMin
	}
	if timing.intervalMax < timing.intervalMin {
		timing.intervalMax = max(defaults.intervalMax, timing.intervalMin)
	}
	if timing.initialInterval <= 0 {
		timing.initialInterval = defaults.initialInterval
	}
	if timing.initialAnnounces <= 0 {
		timing.initialAnnounces = defaults.initialAnnounces
	}
	if timing.hintLifetimeMin <= 0 {
		timing.hintLifetimeMin = defaults.hintLifetimeMin
	}
	if timing.hintLifetimeMax < timing.hintLifetimeMin {
		timing.hintLifetimeMax = max(defaults.hintLifetimeMax, timing.hintLifetimeMin)
	}
	return timing
}

func waitForProviderRetry(ctx context.Context, delay time.Duration) bool {
	select {
	case <-time.After(delay):
		return true
	case <-ctx.Done():
		return false
	}
}

func exponentialTimeout(base time.Duration, attempt int) time.Duration {
	for range attempt {
		if base > time.Duration(1<<63-1)/2 {
			return time.Duration(1<<63 - 1)
		}
		base *= 2
	}
	return base
}

func clampDuration(value, minimum, maximum time.Duration) time.Duration {
	return min(max(value, minimum), maximum)
}

func usableTrackerAddress(address netip.Addr) bool {
	return address.IsValid() && !address.IsUnspecified() && !address.IsMulticast()
}

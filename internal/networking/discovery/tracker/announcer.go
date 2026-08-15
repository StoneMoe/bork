package tracker

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
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
	maxProviderURLLength    = 2048
	maxResolvedAddresses    = 8
	trackerStopTimeout      = 3 * time.Second
	maxTrackerErrorLength   = 512
	initialAnnounceCount    = 3
	initialAnnounceInterval = 5 * time.Second
	maximumAnnounceInterval = 30 * time.Second
)

type provider struct {
	display     string
	scope       string
	announceURL url.URL
}

type announcerTiming struct {
	httpRequestTimeout time.Duration
	providerRetry      time.Duration
	providerRetryMax   time.Duration
}

var defaultAnnouncerTiming = announcerTiming{
	httpRequestTimeout: 20 * time.Second,
	providerRetry:      5 * time.Second,
	providerRetryMax:   5 * time.Minute,
}

// Announcer discovers peers from independently supervised HTTP(S) trackers.
// Registration identities are shared by every address announced to the same
// provider during a room session.
type Announcer struct {
	providers   []provider
	infoHash    [20]byte
	identityKey [32]byte
	httpClient  *http.Client
	logger      *slog.Logger

	lookupNetIP func(context.Context, string, string) ([]netip.Addr, error)
	timing      announcerTiming
	waitRetry   func(context.Context, time.Duration) bool

	candidate AnnounceCandidate
	running   atomic.Bool
	statusMu  sync.RWMutex
	// Group creates one immutable-candidate Announcer per candidate.
	statuses      map[string]ProviderStatus
	statusChanges chan struct{}
}

type ProviderStatus struct {
	Provider        string   `json:"provider"`
	Candidate       string   `json:"candidate"`
	ObservedAddress string   `json:"observedAddress,omitempty"`
	NextAnnounce    string   `json:"nextAnnounce,omitempty"`
	PeerCount       int      `json:"peerCount"`
	PeerAddresses   []string `json:"peerAddresses"`
	Error           string   `json:"error,omitempty"`
}

func (s ProviderStatus) Clone() ProviderStatus {
	s.PeerAddresses = append([]string{}, s.PeerAddresses...)
	return s
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

func ValidateProviderURL(raw string) error {
	_, err := parseProvider(raw)
	return err
}

func newAnnouncerFromProviders(providers []provider, infoHash [20]byte, identityKey [32]byte, candidate AnnounceCandidate, logger *slog.Logger) (*Announcer, error) {
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
		httpClient:  newHTTPClient(),
		logger:      logger,
		lookupNetIP: func(ctx context.Context, network, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, network, host)
		},
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

func (a *Announcer) Snapshot() []ProviderStatus {
	a.statusMu.RLock()
	defer a.statusMu.RUnlock()
	statuses := make([]ProviderStatus, 0, len(a.providers))
	for _, configured := range a.providers {
		status := a.statuses[configured.scope].Clone()
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
	if !usablePeer(netip.AddrPortFrom(candidate.Address, candidate.Port)) {
		return AnnounceCandidate{}, false
	}
	return candidate, true
}

func (a *Announcer) registration(configured provider, candidate AnnounceCandidate) trackerRegistration {
	info := "bork/tracker-registration/v2\x00" + configured.scope
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
		announced, err := a.runHTTPTracker(ctx, configured, hints, timing)
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

func publishAndWait(ctx context.Context, hints chan<- discovery.Hint, response announceResponse, interval time.Duration) error {
	// Keep the old hint alive while the next announce is in flight.
	expiresAt := time.Now().Add(max(interval*2, time.Minute))
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

// Refresh quickly even when a tracker asks clients to wait longer. Fast early
// announces reduce the time needed for two peers entering a room to meet.
func effectiveAnnounceInterval(providerInterval time.Duration, announces int) time.Duration {
	if announces < initialAnnounceCount {
		return initialAnnounceInterval
	}
	return min(max(providerInterval, initialAnnounceInterval), maximumAnnounceInterval)
}

func parseProvider(raw string) (provider, error) {
	if raw == "" || raw != strings.TrimSpace(raw) || len(raw) > maxProviderURLLength {
		return provider{}, errors.New("tracker URL is invalid")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || parsed.Host == "" {
		return provider{}, errors.New("tracker URL must use http:// or https://")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return provider{}, errors.New("tracker URL must use http:// or https://")
	}
	if parsed.User != nil || parsed.Fragment != "" || strings.Contains(raw, "#") {
		return provider{}, errors.New("tracker URL contains unsupported credentials or fragment")
	}
	if parsed.Hostname() == "" {
		return provider{}, errors.New("tracker URL requires a host")
	}
	portText := parsed.Port()
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
	return provider{display: displayURL.String(), scope: scopeURL.String(), announceURL: *parsed}, nil
}

func (a *Announcer) recordSuccess(configured provider, candidate AnnounceCandidate, response announceResponse, interval time.Duration) {
	now := time.Now().UTC()
	observed := observedRegistrationAddress(response, candidate)
	addresses := make([]string, len(response.peers))
	for index, peer := range response.peers {
		addresses[index] = peer.String()
	}
	a.recordStatus(configured, ProviderStatus{
		Provider: configured.display, Candidate: candidate.String(),
		NextAnnounce: now.Add(interval).Format(time.RFC3339), PeerCount: len(response.peers), PeerAddresses: addresses, ObservedAddress: observed,
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
		NextAnnounce: retryAt.UTC().Format(time.RFC3339), PeerAddresses: []string{}, Error: message,
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
	if timing.httpRequestTimeout <= 0 {
		timing.httpRequestTimeout = defaults.httpRequestTimeout
	}
	if timing.providerRetry <= 0 {
		timing.providerRetry = defaults.providerRetry
	}
	if timing.providerRetryMax < timing.providerRetry {
		timing.providerRetryMax = max(defaults.providerRetryMax, timing.providerRetry)
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

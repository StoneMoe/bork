package tracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"bork/internal/networking/discovery"
)

const (
	maxHTTPRequestURLLength = 4096
	maxHTTPRedirects        = 3
	maxHTTPResponseHeaders  = int64(64 << 10)
)

var trackerHTTPTransport = func() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxResponseHeaderBytes = maxHTTPResponseHeaders
	return transport
}()

var generatedHTTPQueryKeys = map[string]struct{}{
	"info_hash":  {},
	"peer_id":    {},
	"port":       {},
	"uploaded":   {},
	"downloaded": {},
	"left":       {},
	"key":        {},
	"compact":    {},
	"numwant":    {},
	"event":      {},
	"ip":         {},
	"nonce":      {},
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Transport:     trackerHTTPTransport,
		CheckRedirect: trackerRedirectPolicy,
	}
}

func (a *Announcer) runHTTPTracker(ctx context.Context, configured provider, hints chan<- discovery.Hint) (bool, error) {
	candidate := a.candidate
	registration := a.registration(configured, candidate)
	announces := 0
	defer a.stopHTTPRegistration(configured, registration)
	for {
		event := ""
		if announces == 0 {
			event = "started"
		}
		response, err := a.httpAnnounce(ctx, configured, registration, event)
		if err != nil {
			return announces > 0, fmt.Errorf("announce to %s: %w", configured.display, err)
		}
		interval := effectiveAnnounceInterval(response.interval, announces)
		announces++
		a.recordSuccess(configured, response, interval)
		if err := publishAndWait(ctx, hints, response, interval); err != nil {
			return true, err
		}
	}
}

func (a *Announcer) httpAnnounce(
	ctx context.Context,
	configured provider,
	registration trackerRegistration,
	event string,
) (announceResponse, error) {
	if err := ctx.Err(); err != nil {
		return announceResponse{}, err
	}
	requestURL, err := a.buildHTTPAnnounceURL(configured, registration, event)
	if err != nil {
		return announceResponse{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, httpRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, requestURL, nil)
	if err != nil {
		return announceResponse{}, fmt.Errorf("build HTTP tracker request: %w", err)
	}
	request.Header.Set("Accept", "text/plain, application/octet-stream")
	request.Header.Set("Cache-Control", "no-cache")
	// Cloudflare challenges Go's default User-Agent before an announce reaches the Worker.
	request.Header.Set("User-Agent", "Bork")

	response, err := a.httpClient.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if ctx.Err() != nil {
			return announceResponse{}, ctx.Err()
		}
		return announceResponse{}, fmt.Errorf("HTTP tracker request: %w", httpErrorWithoutURL(err))
	}
	if response == nil || response.Body == nil {
		return announceResponse{}, errors.New("HTTP tracker returned no response body")
	}
	defer response.Body.Close()
	if ctx.Err() != nil {
		return announceResponse{}, ctx.Err()
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return announceResponse{}, fmt.Errorf("HTTP tracker returned status %d", response.StatusCode)
	}
	if response.ContentLength > maxHTTPTrackerResponseSize {
		return announceResponse{}, fmt.Errorf("HTTP tracker response exceeds %d bytes", maxHTTPTrackerResponseSize)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxHTTPTrackerResponseSize+1))
	if err != nil {
		if ctx.Err() != nil {
			return announceResponse{}, ctx.Err()
		}
		return announceResponse{}, fmt.Errorf("read HTTP tracker response: %w", err)
	}
	if len(body) > maxHTTPTrackerResponseSize {
		return announceResponse{}, fmt.Errorf("HTTP tracker response exceeds %d bytes", maxHTTPTrackerResponseSize)
	}
	if ctx.Err() != nil {
		return announceResponse{}, ctx.Err()
	}
	parsed, err := parseHTTPAnnounceResponse(body)
	if err != nil {
		return announceResponse{}, err
	}
	parsed = a.resolveHTTPPeerNames(requestCtx, parsed)
	if ctx.Err() != nil {
		return announceResponse{}, ctx.Err()
	}
	// Keep any literal or compact peers already returned by the tracker. When
	// the response only contains DNS names, a lookup timeout must be retried
	// instead of turning into an empty result for the full announce interval.
	if err := requestCtx.Err(); err != nil && len(parsed.peers) == 0 {
		return announceResponse{}, fmt.Errorf("resolve HTTP tracker peers: %w", err)
	}
	return parsed, nil
}

func (a *Announcer) resolveHTTPPeerNames(ctx context.Context, response announceResponse) announceResponse {
	seen := make(map[netip.AddrPort]struct{}, maxAnnouncePeers)
	for _, peer := range response.peers {
		seen[peer] = struct{}{}
	}
	for _, peer := range response.peerNames {
		if len(response.peers) >= maxAnnouncePeers {
			break
		}
		addresses, _ := a.lookupNetIP(ctx, "ip", peer.name)
		for _, address := range addresses[:min(len(addresses), maxResolvedAddresses)] {
			response.peers = appendUniquePeer(response.peers, seen, netip.AddrPortFrom(address.Unmap(), peer.port))
		}
	}
	response.peerNames = nil
	return response
}

func (a *Announcer) buildHTTPAnnounceURL(configured provider, registration trackerRegistration, event string) (string, error) {
	endpoint := configured.announceURL
	query, err := url.ParseQuery(endpoint.RawQuery)
	if err != nil {
		return "", errors.New("configured HTTP tracker query is invalid")
	}
	query.Set("info_hash", string(a.infoHash[:]))
	query.Set("peer_id", string(registration.peerID[:]))
	query.Set("port", strconv.FormatUint(uint64(registration.candidate.Port), 10))
	if registration.candidate.Address.IsValid() {
		query.Set("ip", registration.candidate.Address.String())
	} else {
		query.Del("ip")
	}
	query.Set("uploaded", "0")
	query.Set("downloaded", "0")
	query.Set("left", "0")
	query.Set("key", strconv.FormatUint(uint64(registration.key), 10))
	query.Set("compact", "1")
	query.Set("numwant", strconv.Itoa(maxAnnouncePeers))
	// Tracker frontends may cache time-sensitive GET responses despite request no-cache headers.
	query.Set("nonce", strconv.FormatInt(time.Now().UnixNano(), 36))
	if event == "" {
		query.Del("event")
	} else {
		query.Set("event", event)
	}
	endpoint.RawQuery = query.Encode()
	encoded := endpoint.String()
	if len(encoded) > maxHTTPRequestURLLength {
		return "", fmt.Errorf("HTTP tracker request URL exceeds %d bytes", maxHTTPRequestURLLength)
	}
	return encoded, nil
}

func (a *Announcer) stopHTTPRegistration(configured provider, registration trackerRegistration) {
	ctx, cancel := context.WithTimeout(context.Background(), trackerStopTimeout)
	defer cancel()
	if _, err := a.httpAnnounce(ctx, configured, registration, "stopped"); err != nil {
		a.logger.Debug("stop HTTP tracker registration", "provider", configured.display, "candidate", registration.candidate.String(), "error", err)
	}
}

func validateHTTPProviderQuery(rawQuery string) error {
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return errors.New("HTTP tracker URL query is invalid")
	}
	for key := range query {
		if _, generated := generatedHTTPQueryKeys[key]; generated {
			return fmt.Errorf("HTTP tracker URL query duplicates protocol key %q", key)
		}
	}
	return nil
}

func trackerRedirectPolicy(request *http.Request, via []*http.Request) error {
	if len(via) == 0 || len(via) > maxHTTPRedirects {
		return errors.New("HTTP tracker redirect limit exceeded")
	}
	original := via[0].URL
	if request.URL.Scheme != "http" && request.URL.Scheme != "https" {
		return errors.New("HTTP tracker redirected to an unsupported scheme")
	}
	if original.Scheme == "https" && request.URL.Scheme != "https" {
		return errors.New("HTTPS tracker attempted an insecure redirect")
	}
	if !strings.EqualFold(original.Hostname(), request.URL.Hostname()) {
		return errors.New("HTTP tracker attempted a cross-host redirect")
	}
	if request.URL.User != nil || request.URL.Fragment != "" {
		return errors.New("HTTP tracker redirect contains credentials or a fragment")
	}

	originalQuery, err := url.ParseQuery(original.RawQuery)
	if err != nil {
		return errors.New("original HTTP tracker query is invalid")
	}
	redirectQuery, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		return errors.New("redirected HTTP tracker query is invalid")
	}
	for key, values := range originalQuery {
		if redirected, exists := redirectQuery[key]; exists && !slices.Equal(redirected, values) {
			return fmt.Errorf("HTTP tracker redirect changed query key %q", key)
		}
		redirectQuery[key] = append([]string(nil), values...)
	}
	request.URL.RawQuery = redirectQuery.Encode()
	if len(request.URL.String()) > maxHTTPRequestURLLength {
		return errors.New("redirected HTTP tracker URL is too long")
	}
	return nil
}

func httpErrorWithoutURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return err
}

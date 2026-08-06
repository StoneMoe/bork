package tracker

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"

	"bork/internal/networking/discovery"
)

const MaxAnnounceCandidates = 4

type Group struct {
	providers   []provider
	infoHash    [20]byte
	identityKey [32]byte
	transport   Transport
	logger      *slog.Logger

	mu         sync.RWMutex
	candidates []AnnounceCandidate
	revision   uint64
	children   map[AnnounceCandidate]*Announcer
	updates    chan struct{}
	changes    chan struct{}
	running    atomic.Bool
}

func New(providerURLs []string, infoHash [20]byte, identityKey [32]byte, transport Transport, logger *slog.Logger) (*Group, error) {
	providers, err := parseProviders(providerURLs)
	if err != nil {
		return nil, err
	}
	if err := validateProviderConfig(providers, identityKey, transport); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Group{
		providers: providers, infoHash: infoHash, identityKey: identityKey,
		transport: transport, logger: logger, children: make(map[AnnounceCandidate]*Announcer),
		updates: make(chan struct{}, 1), changes: make(chan struct{}, 1),
	}, nil
}

func (g *Group) UpdateCandidates(candidates []AnnounceCandidate) {
	normalized := normalizeAnnounceCandidates(candidates)
	g.mu.Lock()
	if slices.Equal(g.candidates, normalized) {
		g.mu.Unlock()
		return
	}
	g.candidates = normalized
	g.revision++
	g.mu.Unlock()
	select {
	case g.updates <- struct{}{}:
	default:
	}
}

func (g *Group) Snapshot() []ProviderStatus {
	g.mu.RLock()
	candidates := append([]AnnounceCandidate{}, g.candidates...)
	children := make(map[AnnounceCandidate]*Announcer, len(g.children))
	for candidate, child := range g.children {
		children[candidate] = child
	}
	g.mu.RUnlock()
	statuses := make([]ProviderStatus, 0, len(candidates)*len(g.providers))
	for _, candidate := range candidates {
		if child := children[candidate]; child != nil {
			statuses = append(statuses, child.Snapshot()...)
			continue
		}
		for _, configured := range g.providers {
			statuses = append(statuses, ProviderStatus{
				Provider: configured.display, Candidate: candidate.String(), PeerAddresses: []string{},
			})
		}
	}
	return statuses
}

func (g *Group) StatusChanges() <-chan struct{} { return g.changes }

func (g *Group) Run(ctx context.Context, hints chan<- discovery.Hint) error {
	if ctx == nil {
		return errors.New("tracker group context is required")
	}
	if !g.running.CompareAndSwap(false, true) {
		return errors.New("tracker group is already running")
	}
	defer g.running.Store(false)
	for {
		candidates, revision := g.candidateSnapshot()
		if len(candidates) == 0 || len(g.providers) == 0 {
			select {
			case <-g.updates:
				continue
			case <-ctx.Done():
				return nil
			}
		}
		select {
		case <-g.updates:
		default:
		}
		if g.currentRevision() != revision {
			continue
		}
		changed, err := g.runCandidates(ctx, candidates, hints)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}
	}
}

func (g *Group) runCandidates(ctx context.Context, candidates []AnnounceCandidate, hints chan<- discovery.Hint) (bool, error) {
	childCtx, cancel := context.WithCancel(ctx)
	children := make(map[AnnounceCandidate]*Announcer, len(candidates))
	results := make(chan error, len(candidates))
	var workers sync.WaitGroup
	for _, candidate := range candidates {
		child, err := newAnnouncerFromProviders(g.providers, g.infoHash, g.identityKey, candidate, g.transport, g.logger)
		if err != nil {
			cancel()
			workers.Wait()
			return false, err
		}
		children[candidate] = child
		workers.Add(2)
		go func() {
			defer workers.Done()
			results <- child.Run(childCtx, hints)
		}()
		go func() {
			defer workers.Done()
			for {
				select {
				case <-child.StatusChanges():
					g.signalChange()
				case <-childCtx.Done():
					return
				}
			}
		}()
	}
	g.setChildren(children)
	changed := false
	var runErr error
	select {
	case <-g.updates:
		changed = true
	case <-ctx.Done():
	case runErr = <-results:
		if runErr == nil && ctx.Err() == nil {
			runErr = errors.New("tracker announcer stopped unexpectedly")
		}
	}
	cancel()
	workers.Wait()
	g.setChildren(nil)
	if ctx.Err() != nil {
		return false, nil
	}
	return changed, runErr
}

func (g *Group) candidateSnapshot() ([]AnnounceCandidate, uint64) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append([]AnnounceCandidate{}, g.candidates...), g.revision
}

func (g *Group) currentRevision() uint64 {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.revision
}

func (g *Group) setChildren(children map[AnnounceCandidate]*Announcer) {
	g.mu.Lock()
	if children == nil {
		children = make(map[AnnounceCandidate]*Announcer)
	}
	g.children = children
	g.mu.Unlock()
	g.signalChange()
}

func (g *Group) signalChange() {
	select {
	case g.changes <- struct{}{}:
	default:
	}
}

func normalizeAnnounceCandidates(candidates []AnnounceCandidate) []AnnounceCandidate {
	normalized := make([]AnnounceCandidate, 0, min(len(candidates), MaxAnnounceCandidates))
	seen := make(map[AnnounceCandidate]struct{}, MaxAnnounceCandidates)
	var fallback AnnounceCandidate
	for _, candidate := range candidates {
		candidate, valid := normalizeAnnounceCandidate(candidate)
		if !valid {
			continue
		}
		if !candidate.Address.IsValid() {
			if fallback.Port == 0 {
				fallback = candidate
			}
			continue
		}
		if _, exists := seen[candidate]; exists {
			continue
		}
		seen[candidate] = struct{}{}
		normalized = append(normalized, candidate)
		if len(normalized) == MaxAnnounceCandidates {
			break
		}
	}
	if len(normalized) == 0 && fallback.Port != 0 {
		normalized = append(normalized, fallback)
	}
	return normalized
}

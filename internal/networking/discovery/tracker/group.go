package tracker

import (
	"context"
	"errors"
	"log/slog"
	"maps"
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
	logger      *slog.Logger

	mu         sync.RWMutex
	candidates []AnnounceCandidate
	revision   uint64
	children   map[AnnounceCandidate]*Announcer
	updates    chan struct{}
	changes    chan struct{}
	running    atomic.Bool
}

func New(providerURLs []string, infoHash [20]byte, identityKey [32]byte, logger *slog.Logger) (*Group, error) {
	providers, err := parseProviders(providerURLs)
	if err != nil {
		return nil, err
	}
	if len(providers) > 0 && identityKey == [32]byte{} {
		return nil, errors.New("tracker identity key is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Group{
		providers: providers, infoHash: infoHash, identityKey: identityKey,
		logger: logger, updates: make(chan struct{}, 1), changes: make(chan struct{}, 1),
	}, nil
}

func (g *Group) UpdateCandidates(candidates []AnnounceCandidate) {
	candidates = slices.Clone(candidates)
	g.mu.Lock()
	if slices.Equal(g.candidates, candidates) {
		g.mu.Unlock()
		return
	}
	g.candidates = candidates
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
	children := maps.Clone(g.children)
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
		child := newAnnouncerFromProviders(g.providers, g.infoHash, g.identityKey, candidate, g.logger)
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

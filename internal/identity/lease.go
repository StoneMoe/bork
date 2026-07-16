package identity

import (
	"errors"
	"fmt"
	"sync"
)

var ErrAlreadyActive = errors.New("identity is already active in another Bork instance")

var activeLeases = struct {
	sync.Mutex
	peerIDs map[string]struct{}
}{peerIDs: make(map[string]struct{})}

type Lease struct {
	peerID  string
	release func() error
	once    sync.Once
	err     error
}

func Acquire(identity *LocalIdentity) (*Lease, error) {
	if identity == nil {
		return nil, errors.New("identity is required")
	}
	peerID := identity.PeerID()
	activeLeases.Lock()
	if _, exists := activeLeases.peerIDs[peerID]; exists {
		activeLeases.Unlock()
		return nil, ErrAlreadyActive
	}
	activeLeases.peerIDs[peerID] = struct{}{}
	activeLeases.Unlock()

	release, err := acquirePlatformLease(peerID)
	if err != nil {
		activeLeases.Lock()
		delete(activeLeases.peerIDs, peerID)
		activeLeases.Unlock()
		return nil, fmt.Errorf("acquire identity lease: %w", err)
	}
	return &Lease{peerID: peerID, release: release}, nil
}

func (l *Lease) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		l.err = l.release()
		activeLeases.Lock()
		delete(activeLeases.peerIDs, l.peerID)
		activeLeases.Unlock()
	})
	return l.err
}

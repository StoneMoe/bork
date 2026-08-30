package netfilter

import "sync"

type nativeProcessCoordinator struct {
	mu         sync.Mutex
	owner      uint64
	pinnedPath string
}

var netFilterProcessCoordinator nativeProcessCoordinator

func (coordinator *nativeProcessCoordinator) acquire(token uint64, config nativeConfig) error {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.owner != 0 {
		return ErrNativeOwnerBusy
	}
	if coordinator.pinnedPath != "" && coordinator.pinnedPath != config.normalizedDLLPath {
		return ErrNativeDLLMismatch
	}
	coordinator.owner = token
	return nil
}

func (coordinator *nativeProcessCoordinator) pin(token uint64, config nativeConfig) {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.owner == token && coordinator.pinnedPath == "" {
		coordinator.pinnedPath = config.normalizedDLLPath
	}
}

func (coordinator *nativeProcessCoordinator) release(token uint64) {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.owner == token {
		coordinator.owner = 0
	}
}

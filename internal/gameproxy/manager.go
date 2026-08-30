package gameproxy

import (
	"context"
	"sync"

	"bork/internal/gameproxy/intercept"
)

type Manager struct {
	dependencies managerDependencies

	mu      sync.Mutex
	current *managerRun
	status  Status
	changes chan struct{}
}

type managerRun struct {
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	startResult chan error
	startOnce   sync.Once
	supervisor  supervisorRuntime
	relay       *intercept.Relay
	relayResult <-chan error
}

func NewManager(bridge BridgeFactory) *Manager {
	return newManager(defaultDependencies(bridge))
}

func newManager(dependencies managerDependencies) *Manager {
	dependencies = dependencies.withDefaults()
	supported := dependencies.bridge != nil && dependencies.bridge.Supported()
	state := StateUnsupported
	if supported {
		state = StateInactive
	}
	return &Manager{
		dependencies: dependencies,
		status:       Status{Supported: supported, State: state},
		changes:      make(chan struct{}, 1),
	}
}

func (manager *Manager) Start(ctx context.Context, input StartInput) error {
	normalized, err := normalizeStartInput(input)
	if err != nil {
		return err
	}
	manager.mu.Lock()
	if !manager.status.Supported {
		manager.mu.Unlock()
		return ErrUnsupported
	}
	if manager.current != nil {
		manager.mu.Unlock()
		return ErrActive
	}
	runCtx, cancel := context.WithCancel(ctx)
	run := &managerRun{
		ctx: runCtx, cancel: cancel, done: make(chan struct{}), startResult: make(chan error, 1),
	}
	manager.current = run
	manager.publishLocked(Status{
		Supported: true, State: StateStarting, Directory: normalized.Directory,
	})
	manager.mu.Unlock()
	go manager.run(run, normalized)
	return <-run.startResult
}

func (manager *Manager) Stop() {
	manager.mu.Lock()
	run := manager.current
	if run == nil {
		if manager.status.Supported && manager.status.State == StateFailed {
			status := manager.status
			status.State = StateInactive
			status.Error = ""
			manager.publishLocked(status)
		}
		manager.mu.Unlock()
		return
	}
	status := manager.status
	status.State = StateStopping
	status.Error = ""
	manager.publishLocked(status)
	run.cancel()
	manager.mu.Unlock()
	<-run.done
}

func (manager *Manager) Status() Status {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.status
}

func (manager *Manager) Changes() <-chan struct{} { return manager.changes }

func (manager *Manager) publish(status Status) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.publishLocked(status)
}

func (manager *Manager) publishLocked(status Status) {
	if manager.status == status {
		return
	}
	manager.status = status
	select {
	case manager.changes <- struct{}{}:
	default:
	}
}

func (run *managerRun) completeStart(err error) {
	run.startOnce.Do(func() { run.startResult <- err })
}

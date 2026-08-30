package gameproxy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"

	"bork/internal/gameproxy/intercept"
	"bork/internal/gameproxy/iwan"
)

var errFakeDial = errors.New("fake dial")

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (log *eventLog) add(event string) {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.events = append(log.events, event)
}

func (log *eventLog) snapshot() []string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return append([]string(nil), log.events...)
}

type fakeBridgeFactory struct {
	log          *eventLog
	supported    bool
	availableErr error
	bridge       *fakeBridge
	paths        []string
	mutatePaths  bool
}

func (factory *fakeBridgeFactory) Supported() bool { return factory.supported }

func (factory *fakeBridgeFactory) EnsureAvailable(context.Context) error {
	factory.log.add("bridge-available")
	return factory.availableErr
}

func (factory *fakeBridgeFactory) New(_ context.Context, paths []string) (intercept.Bridge, error) {
	factory.log.add("bridge-new")
	factory.paths = append([]string(nil), paths...)
	if factory.mutatePaths && len(paths) > 0 {
		paths[0] = "mutated.exe"
	}
	return factory.bridge, nil
}

func (factory *fakeBridgeFactory) receivedPaths() []string {
	return append([]string(nil), factory.paths...)
}

type fakeBridge struct {
	log        *eventLog
	started    chan struct{}
	startGate  chan struct{}
	startErr   error
	startState intercept.GenerationState
	failures   chan error
	closed     chan struct{}
	startOnce  sync.Once
	closeOnce  sync.Once
	mu         sync.Mutex
}

func newFakeBridge(log *eventLog) *fakeBridge {
	return &fakeBridge{
		log: log, started: make(chan struct{}), failures: make(chan error, 1), closed: make(chan struct{}),
	}
}

func (bridge *fakeBridge) Start(ctx context.Context, callbacks intercept.Callbacks) error {
	bridge.log.add("bridge-start")
	bridge.mu.Lock()
	bridge.startState = callbacks.GenerationState()
	bridge.mu.Unlock()
	bridge.startOnce.Do(func() { close(bridge.started) })
	if bridge.startGate != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-bridge.startGate:
		}
	}
	return bridge.startErr
}

func (bridge *fakeBridge) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-bridge.failures:
		return err
	case <-bridge.closed:
		return nil
	}
}

func (bridge *fakeBridge) generationState() intercept.GenerationState {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	return bridge.startState
}

func (bridge *fakeBridge) Close() error {
	bridge.log.add("bridge-close")
	bridge.closeOnce.Do(func() { close(bridge.closed) })
	return nil
}

type fakeSupervisor struct {
	log       *eventLog
	mu        sync.Mutex
	status    iwan.Status
	changes   chan struct{}
	waiting   chan struct{}
	waitGate  chan struct{}
	waitErr   error
	startErr  error
	stopCount int
}

func newFakeSupervisor(log *eventLog, status iwan.Status) *fakeSupervisor {
	return &fakeSupervisor{
		log: log, status: status, changes: make(chan struct{}, 1), waiting: make(chan struct{}),
	}
}

func (supervisor *fakeSupervisor) Start(context.Context) error {
	supervisor.log.add("iwan-start")
	return supervisor.startErr
}

func (supervisor *fakeSupervisor) Stop() {
	supervisor.log.add("iwan-stop")
	supervisor.mu.Lock()
	supervisor.stopCount++
	supervisor.mu.Unlock()
}

func (supervisor *fakeSupervisor) WaitReady(ctx context.Context) error {
	supervisor.log.add("iwan-wait")
	select {
	case <-supervisor.waiting:
	default:
		close(supervisor.waiting)
	}
	if supervisor.waitGate != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-supervisor.waitGate:
		}
	}
	return supervisor.waitErr
}

func (supervisor *fakeSupervisor) Status() iwan.Status {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	return supervisor.status
}

func (supervisor *fakeSupervisor) Changes() <-chan struct{} { return supervisor.changes }

func (supervisor *fakeSupervisor) publish(status iwan.Status) {
	supervisor.mu.Lock()
	supervisor.status = status
	supervisor.mu.Unlock()
	select {
	case supervisor.changes <- struct{}{}:
	default:
	}
}

func (supervisor *fakeSupervisor) stopCalls() int {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	return supervisor.stopCount
}

func (*fakeSupervisor) DialTCP(context.Context, netip.AddrPort) (net.Conn, error) {
	return nil, errFakeDial
}

func (*fakeSupervisor) OpenUDP() (net.PacketConn, error) { return nil, errFakeDial }

type fakeMatcher struct{}

func (fakeMatcher) Match(string) (bool, error) { return true, nil }

func newTestManager(log *eventLog, supervisor *fakeSupervisor, bridgeFactory *fakeBridgeFactory) *Manager {
	return newManager(managerDependencies{
		bridge: bridgeFactory,
		scanRules: func(string) (ruleSet, error) {
			log.add("scan")
			paths := []string{"alpha.exe", "zeta.exe"}
			return ruleSet{matcher: fakeMatcher{}, paths: paths, executableCount: len(paths)}, nil
		},
		newSupervisor: func(iwan.Options) (supervisorRuntime, error) {
			log.add("iwan-new")
			return supervisor, nil
		},
	})
}

func validStartInput() StartInput {
	return StartInput{
		Node:      iwan.Node{Server: "127.0.0.1", Username: "user", Password: "secret"},
		Directory: "games", DNS: netip.MustParseAddr("1.1.1.1"),
	}
}

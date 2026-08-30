package iwan

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"time"

	"bork/internal/gameproxy/netstack"
)

type Supervisor struct {
	options     Options
	credentials Credentials
	timings     runtimeTimings

	lifecycle sync.Mutex
	mu        sync.Mutex
	desired   bool
	cancel    context.CancelFunc
	runDone   chan struct{}
	changed   chan struct{}
	status    Status
	active    *netstack.Stack
	nextID    uint64
}

type activation struct {
	id      uint64
	network *netstack.Stack
	address netip.Addr
	mtu     uint16
}

func NewSupervisor(options Options) (*Supervisor, error) {
	return newSupervisor(options, defaultRuntimeTimings())
}

func newSupervisor(options Options, timings runtimeTimings) (*Supervisor, error) {
	normalized, credentials, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}
	if timings.openRetry <= 0 || timings.authTimeout <= 0 || timings.echoInterval <= 0 || timings.liveness <= 0 || timings.restartDelay <= 0 {
		return nil, ErrInvalidOptions
	}
	return &Supervisor{
		options: normalized, credentials: credentials, timings: timings,
		changed: make(chan struct{}, 1), status: Status{State: StateStopped},
	}, nil
}

func (supervisor *Supervisor) Start(ctx context.Context) error {
	supervisor.lifecycle.Lock()
	defer supervisor.lifecycle.Unlock()
	supervisor.mu.Lock()
	if supervisor.desired {
		supervisor.mu.Unlock()
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	supervisor.desired = true
	supervisor.cancel = cancel
	supervisor.runDone = done
	supervisor.publishLocked(Status{State: StateConnecting, Generation: supervisor.nextID})
	supervisor.mu.Unlock()
	go supervisor.run(runCtx, done)
	return nil
}

func (supervisor *Supervisor) Stop() {
	supervisor.lifecycle.Lock()
	defer supervisor.lifecycle.Unlock()
	supervisor.mu.Lock()
	if !supervisor.desired && supervisor.runDone == nil {
		supervisor.publishLocked(Status{State: StateStopped, Generation: supervisor.nextID})
		supervisor.mu.Unlock()
		return
	}
	supervisor.desired = false
	cancel := supervisor.cancel
	done := supervisor.runDone
	if cancel != nil {
		cancel()
	}
	supervisor.mu.Unlock()
	if done != nil {
		<-done
	}
	supervisor.mu.Lock()
	supervisor.publishLocked(Status{State: StateStopped, Generation: supervisor.nextID})
	supervisor.mu.Unlock()
}

func (supervisor *Supervisor) Status() Status {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	return supervisor.status
}

func (supervisor *Supervisor) Changes() <-chan struct{} { return supervisor.changed }

func (supervisor *Supervisor) WaitReady(ctx context.Context) error {
	for {
		supervisor.mu.Lock()
		status := supervisor.status
		changed := supervisor.changed
		desired := supervisor.desired
		supervisor.mu.Unlock()
		if status.State == StateReady {
			return nil
		}
		if status.State == StateFailed && status.Err != nil {
			return status.Err
		}
		if !desired {
			return ErrNotReady
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (supervisor *Supervisor) DialTCP(ctx context.Context, remote netip.AddrPort) (net.Conn, error) {
	network := supervisor.readyStack()
	if network == nil {
		return nil, ErrNotReady
	}
	return network.DialTCP(ctx, remote)
}

func (supervisor *Supervisor) OpenUDP() (net.PacketConn, error) {
	network := supervisor.readyStack()
	if network == nil {
		return nil, ErrNotReady
	}
	return network.OpenUDP()
}

func (supervisor *Supervisor) readyStack() *netstack.Stack {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.status.State != StateReady {
		return nil
	}
	return supervisor.active
}

func (supervisor *Supervisor) run(ctx context.Context, done chan struct{}) {
	defer supervisor.finishRun(done)
	for {
		generationID, ok := supervisor.beginGeneration()
		if !ok {
			return
		}
		current := &generation{
			id: generationID, options: supervisor.options, credentials: supervisor.credentials,
			timings: supervisor.timings, owner: supervisor,
		}
		err := current.run(ctx)
		if ctx.Err() != nil || !supervisor.isDesired() {
			return
		}
		if isTerminalFailure(err) {
			supervisor.fail(generationID, err)
			return
		}
		supervisor.retry(generationID, err)
		timer := time.NewTimer(supervisor.timings.restartDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}

func (supervisor *Supervisor) beginGeneration() (uint64, bool) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if !supervisor.desired {
		return 0, false
	}
	supervisor.nextID++
	id := supervisor.nextID
	supervisor.publishLocked(Status{State: StateAuthenticating, Generation: id})
	return id, true
}

func (supervisor *Supervisor) activate(candidate activation) bool {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if !supervisor.desired || supervisor.status.Generation != candidate.id {
		return false
	}
	supervisor.active = candidate.network
	supervisor.publishLocked(Status{
		State: StateReady, Generation: candidate.id, Address: candidate.address, MTU: candidate.mtu,
	})
	return true
}

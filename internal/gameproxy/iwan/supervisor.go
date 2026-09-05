package iwan

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"time"

	"bork/internal/gameproxy/netstack"
)

const maxDataPathEvents = 256

type Supervisor struct {
	options     Options
	credentials Credentials
	timings     runtimeTimings

	lifecycle       sync.Mutex
	mu              sync.Mutex
	desired         bool
	cancel          context.CancelFunc
	runDone         chan struct{}
	changed         chan struct{}
	status          Status
	active          *netstack.Stack
	dataPath        *dataPathCounters
	dataPathEvents  []DataPathEvent
	droppedDataPath uint64
	nextID          uint64
	everReady       bool
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
	supervisor.everReady = false
	supervisor.dataPath = nil
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
	return supervisor.statusLocked()
}

func (supervisor *Supervisor) statusLocked() Status {
	status := supervisor.status
	if supervisor.dataPath != nil {
		status.DataPath = supervisor.dataPath.snapshot()
	}
	return status
}

func (supervisor *Supervisor) DrainDataPathEvents() (Status, []DataPathEvent, uint64) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	status := supervisor.statusLocked()
	events := append([]DataPathEvent(nil), supervisor.dataPathEvents...)
	dropped := supervisor.droppedDataPath
	supervisor.dataPathEvents = nil
	supervisor.droppedDataPath = 0
	return status, events, dropped
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
		generationID, dataPath, ok := supervisor.beginGeneration()
		if !ok {
			return
		}
		current := &generation{
			id: generationID, options: supervisor.options, credentials: supervisor.credentials,
			timings: supervisor.timings, owner: supervisor, dataPath: dataPath,
		}
		err := current.run(ctx)
		if ctx.Err() != nil || !supervisor.isDesired() {
			supervisor.completeDataPath(generationID, dataPath)
			return
		}
		if isTerminalFailure(err) || !supervisor.wasReady() {
			supervisor.fail(generationID, dataPath, err)
			return
		}
		supervisor.retry(generationID, dataPath, err)
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

func (supervisor *Supervisor) completeDataPath(id uint64, dataPath *dataPathCounters) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	supervisor.completeDataPathLocked(id, dataPath)
	if supervisor.status.Generation == id {
		supervisor.publishLocked(Status{State: StateStopped, Generation: id})
	}
}

func (supervisor *Supervisor) completeDataPathLocked(id uint64, dataPath *dataPathCounters) {
	if supervisor.status.Generation != id || supervisor.status.State != StateReady {
		return
	}
	status := supervisor.status
	status.DataPath = dataPath.snapshot()
	supervisor.appendDataPathEventLocked(DataPathEvent{Phase: DataPathEventEnd, Status: status})
}

func (supervisor *Supervisor) appendDataPathEventLocked(event DataPathEvent) {
	if len(supervisor.dataPathEvents) == maxDataPathEvents {
		copy(supervisor.dataPathEvents, supervisor.dataPathEvents[1:])
		supervisor.dataPathEvents = supervisor.dataPathEvents[:len(supervisor.dataPathEvents)-1]
		supervisor.droppedDataPath++
	}
	supervisor.dataPathEvents = append(supervisor.dataPathEvents, event)
}

func (supervisor *Supervisor) beginGeneration() (uint64, *dataPathCounters, bool) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if !supervisor.desired {
		return 0, nil, false
	}
	supervisor.nextID++
	id := supervisor.nextID
	dataPath := &dataPathCounters{}
	supervisor.dataPath = dataPath
	supervisor.publishLocked(Status{State: StateAuthenticating, Generation: id})
	return id, dataPath, true
}

func (supervisor *Supervisor) activate(candidate activation) bool {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if !supervisor.desired || supervisor.status.Generation != candidate.id {
		return false
	}
	supervisor.active = candidate.network
	supervisor.everReady = true
	status := Status{
		State: StateReady, Generation: candidate.id, Address: candidate.address, MTU: candidate.mtu,
		DataPath: supervisor.dataPath.snapshot(),
	}
	supervisor.appendDataPathEventLocked(DataPathEvent{Phase: DataPathEventStart, Status: status})
	supervisor.publishLocked(status)
	return true
}

func (supervisor *Supervisor) wasReady() bool {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	return supervisor.everReady
}

package iwan

import "bork/internal/gameproxy/netstack"

func (supervisor *Supervisor) deactivate(id uint64, network *netstack.Stack) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.status.Generation == id && supervisor.active == network {
		supervisor.active = nil
	}
}

func (supervisor *Supervisor) retry(id uint64, err error) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.desired && supervisor.status.Generation == id {
		supervisor.publishLocked(Status{State: StateRetrying, Generation: id, Err: err})
	}
}

func (supervisor *Supervisor) fail(id uint64, err error) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.status.Generation != id {
		return
	}
	supervisor.desired = false
	supervisor.publishLocked(Status{State: StateFailed, Generation: id, Err: err})
}

func (supervisor *Supervisor) isDesired() bool {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	return supervisor.desired
}

func (supervisor *Supervisor) finishRun(done chan struct{}) {
	supervisor.mu.Lock()
	if supervisor.runDone == done {
		supervisor.runDone = nil
		supervisor.cancel = nil
		if supervisor.status.State != StateFailed {
			supervisor.desired = false
			supervisor.publishLocked(Status{State: StateStopped, Generation: supervisor.nextID})
		}
	}
	supervisor.mu.Unlock()
	close(done)
}

func (supervisor *Supervisor) publishLocked(status Status) {
	supervisor.status = status
	select {
	case supervisor.changed <- struct{}{}:
	default:
	}
}

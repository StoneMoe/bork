//go:build game_proxy

package gameproxy

import (
	"context"
	"errors"
	"slices"

	"bork/internal/gameproxy/intercept"
	"bork/internal/gameproxy/iwan"
)

func (manager *Manager) updateExecutableCount(run *managerRun, count int) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.current != run {
		return
	}
	status := manager.status
	status.ExecutableCount = count
	manager.publishLocked(status)
}

func (manager *Manager) updateTraffic(run *managerRun, traffic TrafficStats, runtimeStatus iwan.Status) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.current != run || run.ctx.Err() != nil {
		return
	}
	status := manager.status
	if status.State != StateRunning || runtimeStatus.State != iwan.StateReady || runtimeStatus.Generation != status.Generation {
		traffic.UploadRate = 0
		traffic.DownloadRate = 0
	} else {
		status.TrafficHistory = append(slices.Clone(status.TrafficHistory), TrafficSample{
			At: runtimeStatus.Quality.ObservedAt, Generation: runtimeStatus.Generation,
			UploadRate: traffic.UploadRate, DownloadRate: traffic.DownloadRate,
		})
	}
	status.Traffic = traffic
	status.Quality = runtimeStatus.Quality
	manager.publishLocked(status)
}

func (manager *Manager) publishRunning(run *managerRun, runtimeStatus iwan.Status) bool {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.current != run || run.ctx.Err() != nil {
		return false
	}
	manager.appendEventLocked(run, "info", "Game proxy is running")
	status := manager.status
	status.State = StateRunning
	status.Generation = runtimeStatus.Generation
	status.Quality = runtimeStatus.Quality
	status.Error = ""
	manager.publishLocked(status)
	return true
}

func (manager *Manager) updateRuntime(run *managerRun, state State, runtimeStatus iwan.Status) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.current != run || run.ctx.Err() != nil || manager.status.State == StateStopping {
		return
	}
	status := manager.status
	status.State = state
	status.Generation = runtimeStatus.Generation
	status.Quality = runtimeStatus.Quality
	status.Error = run.sanitize(errorString(runtimeStatus.Err))
	manager.publishLocked(status)
}

func (manager *Manager) finishStartFailure(
	run *managerRun,
	failure error,
) {
	if run.ctx.Err() != nil {
		manager.finishStopped(run, run.ctx.Err())
		return
	}
	manager.appendEvent(run, "fatal", "Game proxy startup failed: "+errorString(failure))
	if manager.finishFailure(run, failure) == StateInactive {
		run.completeStart(context.Canceled)
		return
	}
	run.completeStart(failure)
}

func (manager *Manager) finishFailure(
	run *managerRun,
	failure error,
) State {
	failure = manager.cleanup(run, failure)
	manager.finishDataPath(run)
	return manager.finish(run, StateFailed, failure)
}

func (manager *Manager) finishStopped(
	run *managerRun,
	startErr error,
) {
	manager.markStopping(run)
	manager.cleanup(run, nil)
	manager.finishDataPath(run)
	manager.finish(run, StateInactive, nil)
	run.completeStart(startErr)
}

func (manager *Manager) finishDataPath(run *managerRun) {
	if run.dataPath == nil || run.supervisor == nil {
		return
	}
	status := manager.appendDataPathLifecycleEvents(run)
	manager.appendDataPathEvents(run, run.dataPath.finish(status))
}

func (manager *Manager) markStopping(run *managerRun) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.current != run || manager.status.State == StateStopping {
		return
	}
	status := manager.status
	status.State = StateStopping
	status.Error = ""
	manager.publishLocked(status)
}

func (manager *Manager) cleanup(run *managerRun, failure error) error {
	if run.relay != nil {
		generation := intercept.Generation(0)
		if run.supervisor != nil {
			generation = intercept.Generation(run.supervisor.Status().Generation)
		}
		run.relay.SetState(intercept.GenerationState{Generation: generation})
		failure = errors.Join(failure, run.relay.Close())
	}
	if run.supervisor != nil {
		run.supervisor.Stop()
	}
	return failure
}

func (manager *Manager) finish(run *managerRun, state State, err error) State {
	run.cancel()
	var runtimeStatus iwan.Status
	if run.supervisor != nil {
		runtimeStatus = run.supervisor.Status()
	}
	var traffic TrafficStats
	if run.relay != nil {
		traffic = run.relay.Traffic()
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.current != run {
		return manager.status.State
	}
	status := manager.status
	if status.State == StateStopping {
		state = StateInactive
		err = nil
	}
	status.State = state
	status.Quality = runtimeStatus.Quality
	status.Traffic = traffic
	status.Error = run.sanitize(errorString(err))
	manager.current = nil
	manager.publishLocked(status)
	return state
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

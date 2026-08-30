package gameproxy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"bork/internal/gameproxy/intercept"
	"bork/internal/gameproxy/iwan"
)

const (
	managerQueueSize   = 256
	managerIdleTimeout = time.Minute
)

func (manager *Manager) run(run *managerRun, input StartInput) {
	defer close(run.done)
	rules, err := manager.dependencies.scanRules(input.Directory)
	if err != nil {
		manager.finishStartFailure(run, fmt.Errorf("scan executable rules: %w", err))
		return
	}
	if rules.executableCount == 0 {
		manager.finishStartFailure(run, ErrNoExecutables)
		return
	}
	manager.updateExecutableCount(run, rules.executableCount)
	if err := run.ctx.Err(); err != nil {
		manager.finishStopped(run, err)
		return
	}
	if err := manager.dependencies.bridge.EnsureAvailable(run.ctx); err != nil {
		manager.finishStartFailure(run, fmt.Errorf("ensure intercept bridge: %w", err))
		return
	}
	supervisor, err := manager.dependencies.newSupervisor(iwan.Options{Node: input.Node})
	if err != nil {
		manager.finishStartFailure(run, fmt.Errorf("construct iwan supervisor: %w", err))
		return
	}
	run.supervisor = supervisor
	if err := supervisor.Start(run.ctx); err != nil {
		manager.finishStartFailure(run, fmt.Errorf("start iwan supervisor: %w", err))
		return
	}
	if err := supervisor.WaitReady(run.ctx); err != nil {
		if run.ctx.Err() != nil {
			manager.finishStopped(run, run.ctx.Err())
		} else {
			manager.finishStartFailure(run, fmt.Errorf("wait for iwan: %w", err))
		}
		return
	}
	iwanStatus := supervisor.Status()
	if iwanStatus.State != iwan.StateReady {
		manager.finishStartFailure(run, iwan.ErrNotReady)
		return
	}
	bridge, err := manager.dependencies.bridge.New(run.ctx, slices.Clone(rules.paths))
	if err != nil {
		manager.finishStartFailure(run, fmt.Errorf("construct intercept bridge: %w", err))
		return
	}
	relay, err := intercept.New(intercept.Options{
		Bridge: bridge, Rules: rules.matcher, Dialer: supervisor, DNS: input.DNS,
		QueueSize: managerQueueSize, IdleTimeout: managerIdleTimeout, Clock: wallClock{},
	})
	if err != nil {
		failure := errors.Join(fmt.Errorf("construct intercept relay: %w", err), bridge.Close())
		manager.finishStartFailure(run, failure)
		return
	}
	run.relay = relay
	relay.SetState(intercept.GenerationState{Generation: intercept.Generation(iwanStatus.Generation), Ready: true})
	if err := relay.Start(run.ctx); err != nil {
		manager.finishStartFailure(run, fmt.Errorf("start intercept relay: %w", err))
		return
	}
	relayResult := make(chan error, 1)
	run.relayResult = relayResult
	go func() { relayResult <- relay.Run(run.ctx) }()
	select {
	case err := <-relayResult:
		manager.finishStartFailure(run, fmt.Errorf("start intercept relay: %w", err))
		return
	case <-run.ctx.Done():
		manager.finishStopped(run, run.ctx.Err())
		return
	default:
	}
	if !manager.publishRunning(run, iwanStatus.Generation) {
		manager.finishStopped(run, context.Canceled)
		return
	}
	run.completeStart(nil)
	manager.watch(run)
}

func (manager *Manager) watch(run *managerRun) {
	for {
		select {
		case <-run.ctx.Done():
			manager.finishStopped(run, run.ctx.Err())
			return
		case err := <-run.relayResult:
			if run.ctx.Err() != nil {
				manager.finishStopped(run, run.ctx.Err())
			} else {
				manager.finishFailure(run, err)
			}
			return
		case <-run.supervisor.Changes():
			status := run.supervisor.Status()
			switch status.State {
			case iwan.StateReady:
				run.relay.SetState(intercept.GenerationState{Generation: intercept.Generation(status.Generation), Ready: true})
				manager.updateRuntime(run, StateRunning, status)
			case iwan.StateConnecting, iwan.StateAuthenticating, iwan.StateRetrying:
				run.relay.SetState(intercept.GenerationState{Generation: intercept.Generation(status.Generation)})
				manager.updateRuntime(run, StateReconnecting, status)
			case iwan.StateFailed:
				manager.finishFailure(run, status.Err)
				return
			case iwan.StateStopped:
				manager.finishFailure(run, ErrSupervisorStopped)
				return
			}
		}
	}
}

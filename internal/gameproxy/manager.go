//go:build game_proxy

package gameproxy

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"bork/internal/gameproxy/intercept"
)

type Manager struct {
	dependencies managerDependencies

	mu        sync.Mutex
	current   *managerRun
	status    Status
	changes   chan struct{}
	eventHead int
}

const (
	maxConnectionEvents  = 4096
	trafficHistoryWindow = 3 * time.Minute
	maxTrafficSamples    = 180
)

type managerRun struct {
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	startResult chan error
	startOnce   sync.Once
	supervisor  supervisorRuntime
	relay       *intercept.Relay
	relayResult <-chan error
	dataPath    *dataPathEventTracker
	redactions  []string
	updateMu    sync.Mutex
	bridge      intercept.Bridge
	rulePaths   []string
	ruleMatcher intercept.ExecutableMatcher
}

func (manager *Manager) UpdateDirectories(ctx context.Context, directories []string) error {
	directories = normalizeDirectories(directories)
	if len(directories) == 0 {
		return fmt.Errorf("directories: %w", ErrInvalidStartInput)
	}
	rules, err := manager.dependencies.scanRules(directories)
	if err != nil {
		return fmt.Errorf("scan executable rules: %w", err)
	}
	if rules.executableCount == 0 {
		return ErrNoExecutables
	}
	manager.mu.Lock()
	run := manager.current
	status := manager.status
	manager.mu.Unlock()
	if run == nil || (status.State != StateRunning && status.State != StateReconnecting) {
		return ErrNotActive
	}
	run.updateMu.Lock()
	defer run.updateMu.Unlock()
	manager.mu.Lock()
	active := manager.current == run && run.ctx.Err() == nil &&
		(manager.status.State == StateRunning || manager.status.State == StateReconnecting)
	manager.mu.Unlock()
	if !active {
		return ErrNotActive
	}
	updater, ok := run.bridge.(interface {
		UpdateRules(context.Context, []string) error
	})
	if !ok {
		return errors.New("gameproxy: bridge does not support runtime rules")
	}
	transition := unionExecutableMatcher{run.ruleMatcher, rules.matcher}
	if err := run.relay.SetRules(transition); err != nil {
		return fmt.Errorf("prepare relay rules: %w", err)
	}
	if err := updater.UpdateRules(ctx, rules.paths); err != nil {
		restoreErr := run.relay.SetRules(run.ruleMatcher)
		return errors.Join(fmt.Errorf("update intercept rules: %w", err), restoreErr)
	}
	if err := run.relay.SetRules(rules.matcher); err != nil {
		rollbackNativeErr := updater.UpdateRules(run.ctx, run.rulePaths)
		rollbackMatcherErr := run.relay.SetRules(run.ruleMatcher)
		return errors.Join(fmt.Errorf("commit relay rules: %w", err), rollbackNativeErr, rollbackMatcherErr)
	}
	run.rulePaths = slices.Clone(rules.paths)
	run.ruleMatcher = rules.matcher
	manager.mu.Lock()
	if manager.current != run || run.ctx.Err() != nil {
		manager.mu.Unlock()
		return context.Canceled
	}
	status = manager.status
	status.Directories = slices.Clone(directories)
	status.ExecutableCount = rules.executableCount
	manager.publishLocked(status)
	manager.mu.Unlock()
	manager.appendEvent(run, "info", fmt.Sprintf("Updated game directories; found %d executables", rules.executableCount))
	return nil
}

type unionExecutableMatcher [2]intercept.ExecutableMatcher

func (matchers unionExecutableMatcher) Match(path string) (bool, error) {
	for _, matcher := range matchers {
		if matcher == nil {
			continue
		}
		matched, err := matcher.Match(path)
		if err != nil || matched {
			return matched, err
		}
	}
	return false, nil
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
		status:       Status{Supported: supported, State: state, Directories: []string{}, Events: []ConnectionEvent{}},
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
	redactions := []string{normalized.Node.Username, normalized.Node.Password}
	slices.SortFunc(redactions, func(left, right string) int { return len(right) - len(left) })
	run := &managerRun{
		ctx: runCtx, cancel: cancel, done: make(chan struct{}), startResult: make(chan error, 1),
		redactions: redactions,
	}
	manager.current = run
	manager.publishLocked(Status{
		Supported: true, State: StateStarting, Directories: slices.Clone(normalized.Directories),
	})
	manager.appendEventLocked(run, "info", "Scanning configured game directories")
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
	if manager.status.State != StateStopping {
		manager.appendEventLocked(run, "info", "Stopping game proxy")
		status := manager.status
		status.State = StateStopping
		status.Error = ""
		manager.publishLocked(status)
	}
	run.cancel()
	manager.mu.Unlock()
	<-run.done
}

func (manager *Manager) Status() Status {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return cloneStatus(manager.status, manager.eventHead)
}

func (manager *Manager) Changes() <-chan struct{} { return manager.changes }

func (manager *Manager) publish(status Status) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.publishLocked(status)
}

func (manager *Manager) publishLocked(status Status) {
	if status.State != StateRunning {
		status.Quality.RTTMillis = nil
		status.Quality.LossPercent = nil
	}
	if status.State != StateRunning || status.Generation != manager.status.Generation {
		status.Traffic.UploadRate = 0
		status.Traffic.DownloadRate = 0
	}
	first := max(0, len(status.TrafficHistory)-maxTrafficSamples)
	for first < len(status.TrafficHistory) && status.Quality.ObservedAt.Sub(status.TrafficHistory[first].At) >= trafficHistoryWindow {
		first++
	}
	// Reslice before comparing; never compact the previously published backing array.
	status.TrafficHistory = status.TrafficHistory[first:]
	if reflect.DeepEqual(manager.status, status) {
		return
	}
	status.Quality = status.Quality.Clone()
	status.TrafficHistory = slices.Clone(status.TrafficHistory)
	manager.status = status
	if len(status.Events) < maxConnectionEvents {
		manager.eventHead = 0
	}
	select {
	case manager.changes <- struct{}{}:
	default:
	}
}

func (manager *Manager) appendEvent(run *managerRun, level, message string) {
	manager.appendEventValue(run, level, message, true)
}

func (manager *Manager) appendStructuredEvent(run *managerRun, level, message string) {
	manager.appendEventValue(run, level, message, false)
}

func (manager *Manager) appendEventValue(run *managerRun, level, message string, sanitize bool) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.current != run {
		return
	}
	manager.appendEventValueLocked(run, level, message, sanitize)
}

func (manager *Manager) appendEventLocked(run *managerRun, level, message string) {
	manager.appendEventValueLocked(run, level, message, true)
}

func (manager *Manager) appendEventValueLocked(run *managerRun, level, message string, sanitize bool) {
	if sanitize {
		message = run.sanitize(message)
	}
	event := ConnectionEvent{
		At: time.Now().UTC().Format(time.RFC3339), Level: level, Message: message,
	}
	if len(manager.status.Events) < maxConnectionEvents {
		manager.status.Events = append(manager.status.Events, event)
	} else {
		manager.status.Events[manager.eventHead] = event
		manager.eventHead = (manager.eventHead + 1) % maxConnectionEvents
	}
	select {
	case manager.changes <- struct{}{}:
	default:
	}
}

func cloneStatus(status Status, eventHead int) Status {
	status.Quality = status.Quality.Clone()
	status.TrafficHistory = slices.Clone(status.TrafficHistory)
	status.Directories = slices.Clone(status.Directories)
	if len(status.Events) == maxConnectionEvents && eventHead != 0 {
		events := make([]ConnectionEvent, len(status.Events))
		copied := copy(events, status.Events[eventHead:])
		copy(events[copied:], status.Events[:eventHead])
		status.Events = events
	} else {
		status.Events = slices.Clone(status.Events)
	}
	if status.Directories == nil {
		status.Directories = []string{}
	}
	if status.Events == nil {
		status.Events = []ConnectionEvent{}
	}
	if status.TrafficHistory == nil {
		status.TrafficHistory = []TrafficSample{}
	}
	return status
}

func (run *managerRun) completeStart(err error) {
	run.startOnce.Do(func() { run.startResult <- err })
}

func (run *managerRun) sanitize(message string) string {
	for _, value := range run.redactions {
		if value != "" {
			message = strings.ReplaceAll(message, value, "[redacted]")
		}
	}
	return message
}

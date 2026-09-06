//go:build game_proxy

package gameproxy

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"bork/internal/gameproxy/intercept"
	"bork/internal/gameproxy/iwan"
)

func TestManager_qualityPublicationIsolationAndInactiveRates(t *testing.T) {
	manager := newManager(managerDependencies{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	run := &managerRun{ctx: ctx}
	manager.current = run
	manager.status = Status{State: StateRunning, Generation: 1}
	now := time.Now()
	runtimeStatus := iwan.Status{State: iwan.StateReady, Generation: 1, Quality: iwan.LinkQuality{
		ObservedAt: now, RTTMillis: new(12.0), LossPercent: new(25.0),
		History: []iwan.LinkSample{{At: now, Generation: 1, RTTMillis: new(12.0), LossPercent: 25}},
	}}
	traffic := TrafficStats{UploadBytes: 100, DownloadBytes: 200, UploadRate: 30, DownloadRate: 60}
	manager.updateTraffic(run, traffic, runtimeStatus)
	*runtimeStatus.Quality.RTTMillis, *runtimeStatus.Quality.LossPercent, *runtimeStatus.Quality.History[0].RTTMillis = 99, 99, 99
	runtimeStatus.Quality.History[0].Generation = 99
	status := manager.Status()
	wantSample := TrafficSample{At: now, Generation: 1, UploadRate: 30, DownloadRate: 60}
	if status.Traffic != traffic || *status.Quality.RTTMillis != 12 || *status.Quality.LossPercent != 25 ||
		*status.Quality.History[0].RTTMillis != 12 || status.Quality.History[0].Generation != 1 ||
		!slices.Equal(status.TrafficHistory, []TrafficSample{wantSample}) {
		t.Fatal("manager retained mutable publication input")
	}
	*status.Quality.RTTMillis, *status.Quality.LossPercent, *status.Quality.History[0].RTTMillis = 88, 88, 88
	status.Quality.History[0].Generation = 88
	status.TrafficHistory[0].UploadRate = 88
	status = manager.Status()
	if *status.Quality.RTTMillis != 12 || *status.Quality.LossPercent != 25 || *status.Quality.History[0].RTTMillis != 12 ||
		status.Quality.History[0].Generation != 1 || !slices.Equal(status.TrafficHistory, []TrafficSample{wantSample}) {
		t.Fatal("manager returned shared telemetry")
	}
	for _, state := range []State{StateReconnecting, StateStopping, StateInactive, StateFailed} {
		status.State, status.Traffic = state, traffic
		manager.publish(status)
		got := manager.Status()
		if got.Traffic.UploadRate != 0 || got.Traffic.DownloadRate != 0 || got.Traffic.UploadBytes != traffic.UploadBytes ||
			got.Quality.RTTMillis != nil || got.Quality.LossPercent != nil || len(got.Quality.History) != 1 ||
			!slices.Equal(got.TrafficHistory, []TrafficSample{wantSample}) {
			t.Fatalf("%s retained stale rates/current quality or lost totals/history: %#v", state, got)
		}
	}
	status.TrafficHistory[0].UploadRate = 99
	if manager.Status().TrafficHistory[0] != wantSample {
		t.Fatal("manager retained mutable traffic publication input")
	}
	status = manager.Status()
	status.State, status.Generation, status.Traffic = StateRunning, 2, traffic
	manager.publish(status)
	if got := manager.Status(); got.Traffic.UploadRate != 0 || got.Traffic.DownloadRate != 0 || len(got.TrafficHistory) != 1 {
		t.Fatal("coalesced reconnect retained previous-generation rates")
	}
	manager.updateTraffic(run, traffic, iwan.Status{State: iwan.StateRetrying, Generation: 2})
	if got := manager.Status(); got.Traffic.UploadRate != 0 || got.Traffic.DownloadRate != 0 || len(got.TrafficHistory) != 1 {
		t.Fatal("poll before reconnect notification restored stale rates")
	}
	runtimeStatus = iwan.Status{State: iwan.StateReady, Generation: 3, Quality: iwan.LinkQuality{ObservedAt: now.Add(time.Second)}}
	manager.updateTraffic(run, traffic, runtimeStatus)
	if got := manager.Status(); got.Traffic.UploadRate != 0 || got.Traffic.DownloadRate != 0 || len(got.TrafficHistory) != 1 {
		t.Fatal("poll with mismatched generation recorded rates")
	}
	manager.updateRuntime(run, StateReconnecting, runtimeStatus)
	manager.updateTraffic(run, traffic, runtimeStatus)
	if got := manager.Status(); got.Traffic.UploadRate != 0 || got.Traffic.DownloadRate != 0 || len(got.TrafficHistory) != 1 {
		t.Fatal("reconnecting manager recorded rates from a ready runtime")
	}
	manager.updateRuntime(run, StateRunning, runtimeStatus)
	if len(manager.Status().TrafficHistory) != 1 {
		t.Fatal("runtime publication sampled traffic outside the ticker")
	}
	manager.updateTraffic(run, traffic, runtimeStatus)
	runtimeStatus.Quality.ObservedAt = now.Add(2 * time.Second)
	manager.updateTraffic(run, TrafficStats{}, runtimeStatus)
	wantHistory := []TrafficSample{
		wantSample,
		{At: now.Add(time.Second), Generation: 3, UploadRate: 30, DownloadRate: 60},
		{At: now.Add(2 * time.Second), Generation: 3},
	}
	if got := manager.Status(); !slices.Equal(got.TrafficHistory, wantHistory) {
		t.Fatalf("generation gap or real idle sample lost: %#v", got.TrafficHistory)
	}
	cancel()
	manager.updateTraffic(run, traffic, runtimeStatus)
	if !slices.Equal(manager.Status().TrafficHistory, wantHistory) {
		t.Fatal("canceled run appended traffic")
	}
}

func TestManager_watchPollsQualityWithoutReadyLogs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		log := &eventLog{}
		supervisor := newFakeSupervisor(log, iwan.Status{State: iwan.StateReady, Generation: 1})
		factory := &fakeBridgeFactory{log: log, supported: true, bridge: newFakeBridge(log)}
		manager := newTestManager(log, supervisor, factory)
		if err := manager.Start(t.Context(), validStartInput()); err != nil {
			t.Fatal(err)
		}
		defer manager.Stop()
		eventCount := len(manager.Status().Events)
		now := time.Now()
		supervisor.mu.Lock()
		supervisor.status.Quality = iwan.LinkQuality{
			ObservedAt: now, RTTMillis: new(9.0), LossPercent: new(0.0),
			History: []iwan.LinkSample{{At: now, Generation: 1, RTTMillis: new(9.0)}},
		}
		supervisor.mu.Unlock()
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		status := manager.Status()
		if status.Quality.RTTMillis == nil || *status.Quality.RTTMillis != 9 || !status.Quality.ObservedAt.Equal(now) || len(status.Events) != eventCount ||
			!slices.Equal(status.TrafficHistory, []TrafficSample{{At: now, Generation: 1}}) {
			t.Fatalf("polled quality/events = %#v", status)
		}
		failedRuntime := supervisor.Status()
		failedRuntime.State, failedRuntime.Err = iwan.StateFailed, errFakeDial
		failedRuntime.Quality.ObservedAt = now.Add(time.Second)
		supervisor.publish(failedRuntime)
		synctest.Wait()
		failed := manager.Status()
		if failed.State != StateFailed || !failed.Quality.ObservedAt.Equal(failedRuntime.Quality.ObservedAt) || !slices.Equal(failed.TrafficHistory, status.TrafficHistory) {
			t.Fatalf("failure did not preserve final traffic history: %#v", failed)
		}
		time.Sleep(4 * time.Minute)
		if frozen := manager.Status(); !frozen.Quality.ObservedAt.Equal(failed.Quality.ObservedAt) || !slices.Equal(frozen.TrafficHistory, failed.TrafficHistory) {
			t.Fatal("failed history did not freeze at the final quality timestamp")
		}
		manager.Stop()
		status = manager.Status()
		if status.Quality.RTTMillis != nil || status.Quality.LossPercent != nil || len(status.Quality.History) != 1 ||
			!slices.Equal(status.TrafficHistory, failed.TrafficHistory) {
			t.Fatalf("stop did not retain final history with unknown current quality: %#v", status.Quality)
		}
		factory.bridge = newFakeBridge(log)
		supervisor.mu.Lock()
		supervisor.status = iwan.Status{State: iwan.StateReady, Generation: 1}
		supervisor.mu.Unlock()
		if err := manager.Start(t.Context(), validStartInput()); err != nil {
			t.Fatal(err)
		}
		if history := manager.Status().TrafficHistory; history == nil || len(history) != 0 {
			t.Fatalf("manual Start did not clear traffic history: %#v", history)
		}
	})
}

func TestManager_trafficHistoryBoundsAndFinalPublication(t *testing.T) {
	manager := newManager(managerDependencies{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	run := &managerRun{ctx: ctx, cancel: cancel}
	manager.current = run
	manager.status = Status{State: StateRunning, Generation: 1}
	now := time.Now()
	runtimeStatus := iwan.Status{State: iwan.StateReady, Generation: 1}
	traffic := TrafficStats{UploadRate: 10, DownloadRate: 20}
	for index := range maxTrafficSamples + 2 {
		runtimeStatus.Quality.ObservedAt = now.Add(time.Duration(index) * time.Millisecond)
		manager.updateTraffic(run, traffic, runtimeStatus)
	}
	if history := manager.Status().TrafficHistory; len(history) != maxTrafficSamples || !history[0].At.Equal(now.Add(2*time.Millisecond)) {
		t.Fatalf("traffic history count bound = %#v", history)
	}
	published := manager.status.TrafficHistory
	before := slices.Clone(published)
	<-manager.Changes()
	// Only history changes: compaction must not make equality miss the new sample.
	manager.updateTraffic(run, traffic, runtimeStatus)
	if !slices.Equal(published, before) {
		t.Fatal("history update mutated the previous publication")
	}
	select {
	case <-manager.Changes():
	default:
		t.Fatal("history-only update did not notify")
	}
	lastAt := runtimeStatus.Quality.ObservedAt
	runtimeStatus.State = iwan.StateRetrying
	manager.updateRuntime(run, StateReconnecting, runtimeStatus)
	runtimeStatus.Quality.ObservedAt = lastAt.Add(trafficHistoryWindow - time.Millisecond)
	manager.updateTraffic(run, traffic, runtimeStatus)
	if history := manager.Status().TrafficHistory; len(history) != 2 || !history[0].At.Equal(lastAt) || !history[1].At.Equal(lastAt) {
		t.Fatalf("reconnecting poll did not prune expired samples: %#v", history)
	}
	runtimeStatus.Quality.ObservedAt = lastAt.Add(trafficHistoryWindow)
	run.supervisor = newFakeSupervisor(&eventLog{}, runtimeStatus)
	manager.finish(run, StateFailed, errFakeDial)
	if status := manager.Status(); status.State != StateFailed || len(status.TrafficHistory) != 0 || !status.Quality.ObservedAt.Equal(runtimeStatus.Quality.ObservedAt) {
		t.Fatalf("final publication did not prune at the quality timestamp: %#v", status)
	}
}

type drainingTrafficSupervisor struct {
	*fakeSupervisor
	connection net.Conn
}

func (supervisor *drainingTrafficSupervisor) DialTCP(context.Context, netip.AddrPort) (net.Conn, error) {
	return supervisor.connection, nil
}

type drainingTrafficFlow struct {
	net.Conn
	writing chan struct{}
	reset   chan struct{}
	once    sync.Once
}

func (*drainingTrafficFlow) Metadata() intercept.Metadata {
	return intercept.Metadata{
		Generation: 1, NativeID: 1, ExecutablePath: "game.exe",
		OriginalLocal: netip.MustParseAddrPort("127.0.0.1:1234"), OriginalRemote: netip.MustParseAddrPort("127.0.0.1:5678"),
	}
}

func (flow *drainingTrafficFlow) Write(payload []byte) (int, error) {
	close(flow.writing)
	<-flow.reset
	return len(payload), nil
}

func (flow *drainingTrafficFlow) Reset(error) error {
	flow.once.Do(func() { close(flow.reset) })
	return nil
}

func TestManager_reconnectRatesExcludeWritesCompletedDuringDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		native, application := net.Pipe()
		defer application.Close()
		stack, server := net.Pipe()
		defer server.Close()
		flow := &drainingTrafficFlow{Conn: native, writing: make(chan struct{}), reset: make(chan struct{})}
		log := &eventLog{}
		bridge := newFakeBridge(log)
		supervisor := newFakeSupervisor(log, iwan.Status{State: iwan.StateReady, Generation: 1})
		manager := newTestManager(log, supervisor, &fakeBridgeFactory{log: log, supported: true, bridge: bridge})
		manager.dependencies.newSupervisor = func(iwan.Options) (supervisorRuntime, error) {
			return &drainingTrafficSupervisor{fakeSupervisor: supervisor, connection: stack}, nil
		}
		if err := manager.Start(t.Context(), validStartInput()); err != nil {
			t.Fatal(err)
		}
		defer manager.Stop()
		if err := bridge.callbacks.TCP(t.Context(), flow); err != nil {
			t.Fatal(err)
		}
		const bytes = 1024
		if _, err := server.Write(make([]byte, bytes)); err != nil {
			t.Fatal(err)
		}
		<-flow.writing
		synctest.Wait()
		// This old-generation write can finish only after SetState cancels the flow.
		observedAt := time.Now()
		supervisor.publish(iwan.Status{State: iwan.StateReady, Generation: 2, Quality: iwan.LinkQuality{ObservedAt: observedAt}})
		synctest.Wait()
		if status := manager.Status(); status.Generation != 2 || status.State != StateRunning {
			t.Fatalf("reconnect did not finish draining: %#v", status)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		traffic := manager.Status().Traffic
		if traffic.DownloadBytes != bytes || traffic.DownloadRate != 0 || traffic.UploadRate != 0 {
			t.Fatalf("old-generation completion leaked into the new rate: %#v", traffic)
		}
		if history := manager.Status().TrafficHistory; !slices.Equal(history, []TrafficSample{{At: observedAt, Generation: 2}}) {
			t.Fatalf("old-generation completion leaked into traffic history: %#v", history)
		}
	})
}

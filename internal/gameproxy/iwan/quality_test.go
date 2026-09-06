//go:build game_proxy

package iwan

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestLinkQuality_timeWindowsAndHistoricalLoss(t *testing.T) {
	supervisor := &Supervisor{desired: true, status: Status{State: StateReady, Generation: 1}}
	now := time.Now()
	supervisor.recordLinkSample(1, now, new(10.0))
	supervisor.recordLinkSample(1, now.Add(2*time.Second), nil)
	supervisor.recordLinkSample(1, now.Add(29*time.Second), new(20.0))
	supervisor.recordLinkSample(1, now.Add(30*time.Second), nil)
	quality := supervisor.linkQualityLocked(now.Add(32 * time.Second))
	if quality.RTTMillis != nil || quality.LossPercent == nil || *quality.LossPercent != 50 {
		t.Fatalf("rolling current quality = %#v", quality)
	}
	for index, want := range []float64{0, 50, 100.0 / 3, 200.0 / 3} {
		if math.Abs(quality.History[index].LossPercent-want) > 1e-9 {
			t.Fatalf("sample %d loss = %v, want %v", index, quality.History[index].LossPercent, want)
		}
	}
	supervisor.recordLinkSample(1, now.Add(32*time.Second), new(15.0))
	quality = supervisor.linkQualityLocked(now.Add(62*time.Second - time.Nanosecond))
	if quality.RTTMillis == nil || *quality.RTTMillis != 15 || quality.LossPercent == nil || *quality.LossPercent != 0 {
		t.Fatalf("last fresh sample = %#v", quality)
	}
	quality = supervisor.linkQualityLocked(now.Add(62 * time.Second))
	if quality.RTTMillis != nil || quality.LossPercent != nil || len(quality.History) != 5 {
		t.Fatalf("stale current quality = %#v", quality)
	}
	quality = supervisor.linkQualityLocked(now.Add(3 * time.Minute))
	if len(quality.History) != 4 || !quality.History[0].At.Equal(now.Add(2*time.Second)) {
		t.Fatalf("three-minute boundary = %#v", quality.History)
	}
	quality = supervisor.linkQualityLocked(now.Add(3*time.Minute + 32*time.Second))
	if len(quality.History) != 0 || quality.History == nil || !quality.ObservedAt.Equal(now.Add(212*time.Second)) {
		t.Fatalf("expired history = %#v", quality)
	}
}

func TestLinkQuality_reconnectKeepsHistoryButResetsCurrentAndManualStart(t *testing.T) {
	supervisor, err := NewSupervisor(Options{Node: Node{Server: "127.0.0.1", Username: "user", Password: "secret"}})
	if err != nil {
		t.Fatal(err)
	}
	supervisor.desired, supervisor.nextID = true, 1
	supervisor.status = Status{State: StateReady, Generation: 1}
	now := time.Now()
	supervisor.recordLinkSample(1, now, nil)
	supervisor.retry(1, &dataPathCounters{}, ErrInactive)
	quality := supervisor.Status().Quality
	if quality.RTTMillis != nil || quality.LossPercent != nil || len(quality.History) != 1 {
		t.Fatalf("retry quality = %#v", quality)
	}
	id, _, ok := supervisor.beginGeneration()
	if !ok || id != 2 || !supervisor.activate(activation{id: id}) {
		t.Fatal("could not activate replacement generation")
	}
	quality = supervisor.Status().Quality
	if quality.RTTMillis != nil || quality.LossPercent != nil || len(quality.History) != 1 {
		t.Fatalf("new generation without outcomes = %#v", quality)
	}
	supervisor.recordLinkSample(1, now, new(99.0))
	supervisor.recordLinkSample(2, now, new(8.0))
	quality = supervisor.Status().Quality
	if len(quality.History) != 2 || quality.History[0].Generation != 1 || quality.History[1].Generation != 2 ||
		quality.History[1].LossPercent != 0 || quality.LossPercent == nil || *quality.LossPercent != 0 || *quality.RTTMillis != 8 {
		t.Fatalf("replacement quality = %#v", quality)
	}
	supervisor.Stop()
	quality = supervisor.Status().Quality
	if len(quality.History) != 2 || quality.RTTMillis != nil || quality.LossPercent != nil {
		t.Fatalf("stopped quality = %#v", quality)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	supervisor.Stop()
	if quality := supervisor.Status().Quality; len(quality.History) != 0 || quality.RTTMillis != nil || quality.LossPercent != nil {
		t.Fatalf("manual restart quality = %#v", quality)
	}
}

func TestLinkQuality_boundedIsolatedSnapshotsWithoutChanges(t *testing.T) {
	supervisor := &Supervisor{
		desired: true, status: Status{State: StateReady, Generation: 1}, changed: make(chan struct{}, 1),
	}
	now := time.Now()
	rtt := 12.0
	for index := 0; index <= maxLinkSamples; index++ {
		supervisor.recordLinkSample(1, now.Add(time.Duration(index-maxLinkSamples)*time.Millisecond), &rtt)
	}
	rtt = 99
	quality := supervisor.Status().Quality
	if len(quality.History) != maxLinkSamples || !quality.History[0].At.Equal(now.Add(-179*time.Millisecond)) {
		t.Fatalf("bounded history = %d entries", len(quality.History))
	}
	*quality.RTTMillis, *quality.LossPercent, *quality.History[0].RTTMillis = 99, 99, 99
	quality.History[1].Generation = 99
	status, _, _ := supervisor.DrainDataPathEvents()
	if *status.Quality.RTTMillis != 12 || *status.Quality.LossPercent != 0 || *status.Quality.History[0].RTTMillis != 12 || status.Quality.History[1].Generation != 1 {
		t.Fatal("consumer mutated supervisor history through a snapshot")
	}
	*status.Quality.History[0].RTTMillis = 99
	if *supervisor.Status().Quality.History[0].RTTMillis != 12 {
		t.Fatal("drained status shared telemetry pointers")
	}
	select {
	case <-supervisor.Changes():
		t.Fatal("probe telemetry emitted a lifecycle notification")
	default:
	}
}

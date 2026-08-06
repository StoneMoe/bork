package audio

import (
	"context"
	"log/slog"
	"math"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"bork/internal/media"
)

func TestUnexpectedDeviceStopDoesNotWaitForItsMonitor(t *testing.T) {
	engine, run, stopped := testRunningEngine()
	engine.mu.Lock()
	engine.state.Speaking = true
	engine.state.SpeakingPeerIDs = []string{"peer"}
	engine.mu.Unlock()
	stopped <- struct{}{}
	select {
	case <-run.monitorDone:
	case <-time.After(time.Second):
		t.Fatal("unexpected-stop monitor deadlocked")
	}
	if state := engine.Status(); state.Running || state.Speaking || len(state.SpeakingPeerIDs) != 0 {
		t.Fatalf("audio activity remained after device stop: %#v", state)
	}
}

func TestExplicitStopRacingDeviceStopReturns(t *testing.T) {
	engine, _, stopped := testRunningEngine()
	done := make(chan struct{})
	go func() {
		engine.Stop()
		close(done)
	}()
	stopped <- struct{}{}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("explicit Stop deadlocked with device-stop monitor")
	}
}

func TestCapturePlaybackMuteAndStopSemantics(t *testing.T) {
	engine, run, _ := testRunningEngine()
	engine.mu.Lock()
	engine.state.Speaking = true
	engine.state.SpeakingPeerIDs = []string{"peer"}
	engine.mu.Unlock()

	generation := run.captureGeneration.Load()
	engine.SetPlaybackMuted(true)
	if state := engine.Status(); !state.PlaybackMuted || state.CaptureMuted || !state.Speaking || len(state.SpeakingPeerIDs) != 1 {
		t.Fatalf("audio state after playback mute = %#v", state)
	}
	if run.captureGeneration.Load() != generation {
		t.Fatal("playback mute invalidated capture")
	}
	if got := run.audioNotification.Swap(0); got != audioMutedNotification {
		t.Fatalf("playback mute notification = %d", got)
	}
	engine.SetPlaybackMuted(true)
	if got := run.audioNotification.Load(); got != 0 {
		t.Fatalf("unchanged playback mute queued notification %d", got)
	}

	engine.SetCaptureMuted(true)
	if state := engine.Status(); !state.CaptureMuted || !state.PlaybackMuted || state.Speaking || len(state.SpeakingPeerIDs) != 1 {
		t.Fatalf("audio state after capture mute = %#v", state)
	}
	if run.captureGeneration.Load() == generation {
		t.Fatal("capture mute did not invalidate capture")
	}
	if got := run.audioNotification.Swap(0); got != audioMutedNotification {
		t.Fatalf("capture mute notification = %d", got)
	}
	mutedGeneration := run.captureGeneration.Load()
	engine.SetCaptureMuted(true)
	if run.captureGeneration.Load() != mutedGeneration || run.audioNotification.Load() != 0 {
		t.Fatal("unchanged capture mute reset capture or queued a notification")
	}
	engine.SetCaptureMuted(false)
	if got := run.audioNotification.Swap(0); got != audioUnmutedNotification {
		t.Fatalf("capture unmute notification = %d", got)
	}
	engine.SetPlaybackMuted(false)
	if got := run.audioNotification.Swap(0); got != audioUnmutedNotification {
		t.Fatalf("playback unmute notification = %d", got)
	}
	engine.mu.Lock()
	engine.state.Speaking = true
	engine.mu.Unlock()
	engine.Stop()
	if state := engine.Status(); state.Running || state.Speaking || state.SpeakingPeerIDs == nil || len(state.SpeakingPeerIDs) != 0 {
		t.Fatalf("audio state after stop = %#v", state)
	}
}

func TestAudioControlDefaultsAndGainBounds(t *testing.T) {
	state := defaultStatus()
	if state.CaptureMuted || state.PlaybackMuted || state.CaptureGain != 100 || state.PlaybackGain != 100 || !state.EchoCancellation || !state.NoiseSuppression || !state.RemoteLoudnessNormalization {
		t.Fatalf("audio control defaults = %#v", state)
	}
	engine := &Engine{state: state, statusChanges: make(chan struct{}, 1)}
	engine.captureGain.Store(defaultAudioGain)
	engine.playbackGain.Store(defaultAudioGain)
	engine.echoCancellation.Store(true)
	engine.noiseSuppression.Store(true)
	engine.remoteLoudnessNormalization.Store(true)

	for _, gain := range []int{minimumAudioGain, maximumAudioGain} {
		if err := engine.SetCaptureGain(gain); err != nil {
			t.Fatalf("SetCaptureGain(%d) error = %v", gain, err)
		}
		if err := engine.SetPlaybackGain(gain); err != nil {
			t.Fatalf("SetPlaybackGain(%d) error = %v", gain, err)
		}
	}
	for _, gain := range []int{-1, 201} {
		if err := engine.SetCaptureGain(gain); err == nil {
			t.Fatalf("SetCaptureGain(%d) succeeded", gain)
		}
		if err := engine.SetPlaybackGain(gain); err == nil {
			t.Fatalf("SetPlaybackGain(%d) succeeded", gain)
		}
	}
	if status := engine.Status(); status.CaptureGain != maximumAudioGain || status.PlaybackGain != maximumAudioGain {
		t.Fatalf("invalid gain changed state: %#v", status)
	}
	engine.SetRemoteLoudnessNormalization(false)
	engine.SetEchoCancellation(false)
	engine.SetNoiseSuppression(false)
	status := engine.Status()
	if status.RemoteLoudnessNormalization || status.EchoCancellation || status.NoiseSuppression {
		t.Fatalf("audio processing setters did not update state: %#v", status)
	}
}

func TestPeerChangeNotificationsAreFiniteAndDistinct(t *testing.T) {
	joined := make([]float32, notificationSamples+FrameSamples)
	left := make([]float32, len(joined))
	joinTone := notificationTone{}
	leftTone := notificationTone{}
	joinTone.start(peerJoinedNotification)
	leftTone.start(peerLeftNotification)
	joinTone.mix(joined, false)
	leftTone.mix(left, false)

	middle := notificationSamples / 2
	if zeroCrossings(joined[middle:notificationSamples]) <= zeroCrossings(joined[:middle]) {
		t.Fatal("join notification is not ascending")
	}
	if zeroCrossings(left[middle:notificationSamples]) >= zeroCrossings(left[:middle]) {
		t.Fatal("leave notification is not descending")
	}
	unmuted := make([]float32, FrameSamples)
	muted := make([]float32, FrameSamples)
	unmuteTone := notificationTone{}
	muteTone := notificationTone{}
	unmuteTone.start(audioUnmutedNotification)
	muteTone.start(audioMutedNotification)
	unmuteTone.mix(unmuted, false)
	muteTone.mix(muted, false)
	if zeroCrossings(joined[:FrameSamples]) == zeroCrossings(unmuted) || zeroCrossings(left[:FrameSamples]) == zeroCrossings(muted) {
		t.Fatal("room and mute notifications are not distinct")
	}
	for index, sample := range joined {
		if math.Abs(float64(sample)) > notificationAmplitude+1e-6 {
			t.Fatalf("notification sample %d exceeds amplitude: %f", index, sample)
		}
		if index >= notificationSamples && sample != 0 {
			t.Fatalf("notification continued after its duration at sample %d", index)
		}
	}
	fullScaleVoice := make([]float32, notificationSamples)
	for index := range fullScaleVoice {
		fullScaleVoice[index] = 1
	}
	tone := notificationTone{}
	tone.start(peerJoinedNotification)
	tone.mix(fullScaleVoice, false)
	for index, sample := range fullScaleVoice {
		if sample < -1 || sample > 1 {
			t.Fatalf("notification clipped full-scale voice at sample %d: %f", index, sample)
		}
	}
	mutedVoice := make([]float32, notificationSamples)
	for index := range mutedVoice {
		mutedVoice[index] = 0.75
	}
	controlTone := notificationTone{}
	controlTone.start(audioMutedNotification)
	if !controlTone.mix(mutedVoice, true) {
		t.Fatal("mute notification was not marked local-only")
	}
	for index, sample := range mutedVoice {
		if math.Abs(float64(sample)) > notificationAmplitude+1e-6 {
			t.Fatalf("local-only notification leaked voice at sample %d: %f", index, sample)
		}
	}
	peerTone := notificationTone{}
	peerTone.start(peerJoinedNotification)
	if peerTone.mix(make([]float32, FrameSamples), true) {
		t.Fatal("peer notification bypassed playback mute")
	}
}

func TestPeerChangeNotificationOnlyQueuesWhileRunning(t *testing.T) {
	engine, run, _ := testRunningEngine()
	engine.PlayPeerChange(true)
	engine.PlayPeerChange(false)
	if got := run.peerNotification.Swap(0); got != peerLeftNotification {
		t.Fatalf("coalesced notification = %d, want latest", got)
	}
	engine.Stop()
	engine.PlayPeerChange(false)
	if got := run.peerNotification.Load(); got != 0 {
		t.Fatalf("stopped engine queued notification %d", got)
	}
}

func TestStopWithPeerLeaveQueuesToneBeforeStopping(t *testing.T) {
	engine, run, _ := testRunningEngine()
	engine.StopWithPeerLeave()
	if got := run.urgentNotification.Load(); got != peerLeftNotification {
		t.Fatalf("leave notification = %d, want %d", got, peerLeftNotification)
	}
	if engine.Status().Running {
		t.Fatal("engine remained running after leave notification")
	}
}

func TestUrgentLeaveNotificationPreemptsQueuedAudio(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	run := &engineRun{cancel: cancel, port: media.NewFlow(), monitorDone: make(chan struct{})}
	run.active.Store(true)
	run.audioNotification.Store(audioUnmutedNotification)
	run.urgentNotification.Store(peerLeftNotification)
	engine := &Engine{run: run, state: defaultStatus(), statusChanges: make(chan struct{}, 1)}
	engine.playbackGain.Store(defaultAudioGain)
	engine.remoteLoudnessNormalization.Store(true)
	queue := newPCMFrameQueue(1)
	wake := make(chan struct{}, 1)
	var demand atomic.Uint64
	demand.Store(1)
	run.workers.Add(1)
	go engine.playbackLoop(ctx, run, queue, wake, &demand)
	defer func() {
		cancel()
		run.workers.Wait()
	}()
	wake <- struct{}{}

	var got []float32
	deadline := time.After(time.Second)
	for got == nil {
		if frame, ok := queue.AcquireRead(); ok {
			got = slices.Clone(frame.Samples[:])
			queue.ReleaseRead()
			break
		}
		select {
		case <-deadline:
			t.Fatal("playback loop did not render the leave notification")
		case <-time.After(time.Millisecond):
		}
	}
	want := make([]float32, FrameSamples)
	tone := notificationTone{}
	tone.start(peerLeftNotification)
	tone.mix(want, false)
	if !slices.Equal(got, want) {
		t.Fatal("queued audio notification played before urgent leave notification")
	}
	demand.Store(4)
	wake <- struct{}{}
	got = nil
	deadline = time.After(time.Second)
	for got == nil {
		if frame, ok := queue.AcquireRead(); ok {
			got = slices.Clone(frame.Samples[:])
			queue.ReleaseRead()
			break
		}
		select {
		case <-deadline:
			t.Fatal("playback loop did not render the next leave notification frame")
		case <-time.After(time.Millisecond):
		}
	}
	clear(want)
	tone.mix(want, false)
	if !slices.Equal(got, want) {
		t.Fatal("discarded catch-up frames consumed the leave notification")
	}
}

func zeroCrossings(samples []float32) int {
	crossings := 0
	previous := float32(0)
	for _, sample := range samples {
		if sample != 0 && previous != 0 && (sample > 0) != (previous > 0) {
			crossings++
		}
		if sample != 0 {
			previous = sample
		}
	}
	return crossings
}

func TestStaleCaptureGenerationCannotReactivateSpeaking(t *testing.T) {
	run := &engineRun{}
	run.active.Store(true)
	run.captureGeneration.Store(2)
	engine := &Engine{state: Status{SpeakingPeerIDs: []string{}}, statusChanges: make(chan struct{}, 1)}

	engine.setLocalSpeaking(run, 1, true)
	if engine.Status().Speaking {
		t.Fatal("stale capture generation reactivated speaking")
	}
	select {
	case <-engine.StatusChanges():
		t.Fatal("stale capture generation published a status change")
	default:
	}

	engine.setLocalSpeaking(run, 2, true)
	if !engine.Status().Speaking {
		t.Fatal("current capture generation did not activate speaking")
	}
	<-engine.StatusChanges()
	engine.setLocalSpeaking(run, 2, true)
	select {
	case <-engine.StatusChanges():
		t.Fatal("unchanged local speaking state was published")
	default:
	}
}

func TestSpeakingPeerIDsSnapshotAndTransitionPublishing(t *testing.T) {
	run := &engineRun{}
	run.active.Store(true)
	engine := &Engine{state: Status{SpeakingPeerIDs: []string{}}, statusChanges: make(chan struct{}, 1)}

	engine.setSpeakingPeerIDs(run, []string{"peer-a", "peer-b"})
	status := engine.Status()
	status.SpeakingPeerIDs[0] = "changed"
	if got := engine.Status().SpeakingPeerIDs[0]; got != "peer-a" {
		t.Fatalf("status snapshot mutated engine state: %q", got)
	}
	<-engine.StatusChanges()
	engine.setSpeakingPeerIDs(run, []string{"peer-a", "peer-b"})
	select {
	case <-engine.StatusChanges():
		t.Fatal("unchanged speaking peer IDs were published")
	default:
	}
	if cloneStatus(Status{}).SpeakingPeerIDs == nil {
		t.Fatal("empty speaking peer IDs snapshot is nil")
	}
}

func testRunningEngine() (*Engine, *engineRun, chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	run := &engineRun{cancel: cancel, port: media.NewFlow(), monitorDone: make(chan struct{})}
	run.active.Store(true)
	state := defaultStatus()
	state.Running = true
	engine := &Engine{
		logger:        slog.Default(),
		run:           run,
		state:         state,
		statusChanges: make(chan struct{}, 1),
	}
	engine.captureGain.Store(defaultAudioGain)
	engine.playbackGain.Store(defaultAudioGain)
	engine.echoCancellation.Store(true)
	engine.noiseSuppression.Store(true)
	engine.remoteLoudnessNormalization.Store(true)
	stopped := make(chan struct{}, 1)
	go engine.watchDeviceStop(ctx, run, stopped)
	return engine, run, stopped
}

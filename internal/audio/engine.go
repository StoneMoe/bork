package audio

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"bork/internal/media"

	"github.com/gen2brain/malgo"
)

const (
	captureQueueFrames      = 2
	playbackQueueFrames     = 2
	voiceSendBudget         = 20 * time.Millisecond
	maxPlaybackAge          = 100 * time.Millisecond
	audioRerouteGracePeriod = 500 * time.Millisecond

	peerJoinedNotification   = 1
	peerLeftNotification     = 2
	audioMutedNotification   = 3
	audioUnmutedNotification = 4
	notificationSamples      = SampleRate * 140 / 1000
	notificationAttack       = SampleRate * 5 / 1000
	notificationRelease      = SampleRate * 30 / 1000
	notificationAmplitude    = 0.15
)

type engineRun struct {
	cancel context.CancelFunc
	port   media.AudioPort

	device            *malgo.Device
	workers           sync.WaitGroup
	captureGeneration atomic.Uint64
	peerNotification  atomic.Uint32
	audioNotification atomic.Uint32
	active            atomic.Bool

	monitorDone chan struct{}
}

type Engine struct {
	logger               *slog.Logger
	context              *malgo.AllocatedContext
	maxEncodedFrameBytes int

	opMu  sync.Mutex
	mu    sync.RWMutex
	state Status

	run                         *engineRun
	captureMuted                atomic.Bool
	playbackMuted               atomic.Bool
	captureGain                 atomic.Int64
	playbackGain                atomic.Int64
	echoCancellation            atomic.Bool
	noiseSuppression            atomic.Bool
	remoteLoudnessNormalization atomic.Bool
	statusChanges               chan struct{}
	closed                      bool
}

func New(options Options, logger *slog.Logger) (*Engine, error) {
	if options.MaxEncodedFrameBytes <= 0 {
		return nil, errors.New("maximum encoded frame size must be positive")
	}
	if logger == nil {
		logger = slog.Default()
	}
	contextConfig := malgo.ContextConfig{}
	audioContext, err := malgo.InitContext(preferredBackends(), contextConfig, nil)
	if err != nil {
		return nil, fmt.Errorf("initialise audio context: %w", err)
	}
	engine := &Engine{
		logger:               logger,
		context:              audioContext,
		maxEncodedFrameBytes: options.MaxEncodedFrameBytes,
		state:                defaultStatus(),
		statusChanges:        make(chan struct{}, 1),
	}
	engine.captureGain.Store(defaultAudioGain)
	engine.playbackGain.Store(defaultAudioGain)
	engine.echoCancellation.Store(true)
	engine.noiseSuppression.Store(true)
	engine.remoteLoudnessNormalization.Store(true)
	if _, err := engine.refreshDevicesLocked(); err != nil {
		_ = audioContext.Uninit()
		audioContext.Free()
		return nil, err
	}
	return engine, nil
}

func preferredBackends() []malgo.Backend {
	switch runtime.GOOS {
	case "windows":
		return []malgo.Backend{malgo.BackendWasapi}
	case "darwin":
		return []malgo.Backend{malgo.BackendCoreaudio}
	default:
		return nil
	}
}

func (e *Engine) StatusChanges() <-chan struct{} {
	return e.statusChanges
}

func (e *Engine) Status() Status {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return cloneStatus(e.state)
}

func (e *Engine) RefreshDevices() error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if e.closed {
		return errors.New("audio engine is closed")
	}
	selectionChanged, err := e.refreshDevicesLocked()
	if err != nil {
		return err
	}
	if e.run == nil {
		return nil
	}
	e.mu.RLock()
	available := e.state.Available
	e.mu.RUnlock()
	if !selectionChanged && available {
		return nil
	}
	return e.rebuildRunLocked(e.run)
}

func (e *Engine) refreshDevicesLocked() (bool, error) {
	capture, err := enumerateDevices(e.context.Context, malgo.Capture)
	if err != nil {
		return false, fmt.Errorf("enumerate capture devices: %w", err)
	}
	playback, err := enumerateDevices(e.context.Context, malgo.Playback)
	if err != nil {
		return false, fmt.Errorf("enumerate playback devices: %w", err)
	}
	e.mu.Lock()
	previousCaptureID := e.state.CaptureDeviceID
	previousPlaybackID := e.state.PlaybackDeviceID
	nextCaptureID := availableDeviceID(capture, previousCaptureID)
	nextPlaybackID := availableDeviceID(playback, previousPlaybackID)
	selectionChanged := previousCaptureID != nextCaptureID || previousPlaybackID != nextPlaybackID
	available := len(capture) > 0 && len(playback) > 0
	unchanged := e.state.CaptureDeviceID == nextCaptureID && e.state.PlaybackDeviceID == nextPlaybackID &&
		e.state.Available == available && slices.Equal(e.state.CaptureDevices, capture) && slices.Equal(e.state.PlaybackDevices, playback)
	if unchanged {
		e.mu.Unlock()
		return false, nil
	}
	e.state.CaptureDevices = capture
	e.state.PlaybackDevices = playback
	e.state.Available = available
	e.state.CaptureDeviceID = nextCaptureID
	e.state.PlaybackDeviceID = nextPlaybackID
	e.mu.Unlock()
	e.publish()
	return selectionChanged, nil
}

func enumerateDevices(audioContext malgo.Context, kind malgo.DeviceType) ([]Device, error) {
	infos, err := audioContext.Devices(kind)
	if err != nil {
		return nil, err
	}
	devices := make([]Device, 0, len(infos))
	for index := range infos {
		devices = append(devices, Device{
			ID:        infos[index].ID.String(),
			Name:      infos[index].Name(),
			IsDefault: infos[index].IsDefault != 0,
		})
	}
	return devices, nil
}

func deviceExists(devices []Device, id string) bool {
	if id == "" {
		return true
	}
	for _, device := range devices {
		if device.ID == id {
			return true
		}
	}
	return false
}

func availableDeviceID(devices []Device, selected string) string {
	if deviceExists(devices, selected) {
		return selected
	}
	return ""
}

func (e *Engine) SetDevices(captureID, playbackID string) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if e.closed {
		return errors.New("audio engine is closed")
	}
	e.mu.RLock()
	if err := validateDeviceIDs(e.state.CaptureDevices, e.state.PlaybackDevices, captureID, playbackID); err != nil {
		e.mu.RUnlock()
		return err
	}
	unchanged := e.state.CaptureDeviceID == captureID && e.state.PlaybackDeviceID == playbackID
	e.mu.RUnlock()
	if unchanged {
		return nil
	}
	run := e.run
	e.mu.Lock()
	e.state.CaptureDeviceID = captureID
	e.state.PlaybackDeviceID = playbackID
	e.mu.Unlock()
	if run == nil {
		e.publish()
		return nil
	}
	return e.rebuildRunLocked(run)
}

func validateDeviceIDs(capture, playback []Device, captureID, playbackID string) error {
	if !deviceExists(capture, captureID) {
		return errors.New("capture device is unavailable")
	}
	if !deviceExists(playback, playbackID) {
		return errors.New("playback device is unavailable")
	}
	return nil
}

func (e *Engine) SetCaptureMuted(muted bool) {
	e.setCaptureMuted(muted, true)
}

// SetCaptureMutedQuietly changes the capture gate without playing the normal
// mute tone. Push-to-talk uses it for every press and release.
func (e *Engine) SetCaptureMutedQuietly(muted bool) {
	e.setCaptureMuted(muted, false)
}

func (e *Engine) setCaptureMuted(muted, notify bool) {
	e.opMu.Lock()
	e.mu.RLock()
	unchanged := e.state.CaptureMuted == muted
	e.mu.RUnlock()
	if unchanged {
		e.opMu.Unlock()
		return
	}
	if muted {
		e.captureMuted.Store(true)
	}
	if e.run != nil {
		generation := e.run.port.InvalidateSend()
		e.run.captureGeneration.Store(generation)
	}
	if !muted {
		e.captureMuted.Store(false)
	}
	e.mu.Lock()
	e.state.CaptureMuted = muted
	if muted && e.state.Speaking {
		e.state.Speaking = false
	}
	if muted {
		e.state.CaptureLevel = 0
		e.state.CaptureClipped = false
	}
	e.mu.Unlock()
	if notify {
		e.queueAudioNotificationLocked(muted)
	}
	e.publish()
	e.opMu.Unlock()
}

func (e *Engine) SetPlaybackMuted(muted bool) {
	e.opMu.Lock()
	e.playbackMuted.Store(muted)
	e.mu.Lock()
	changed := e.state.PlaybackMuted != muted
	e.state.PlaybackMuted = muted
	e.mu.Unlock()
	if changed {
		e.queueAudioNotificationLocked(muted)
	}
	if changed {
		e.publish()
	}
	e.opMu.Unlock()
}

func (e *Engine) queueAudioNotificationLocked(muted bool) {
	if e.run == nil || !e.run.active.Load() {
		return
	}
	notification := uint32(audioUnmutedNotification)
	if muted {
		notification = audioMutedNotification
	}
	e.run.audioNotification.Store(notification)
}

// PlayPeerChange queues a local tone on the current playback device.
func (e *Engine) PlayPeerChange(joined bool) {
	notification := uint32(peerLeftNotification)
	if joined {
		notification = peerJoinedNotification
	}
	e.opMu.Lock()
	if e.run != nil && e.run.active.Load() {
		// ponytail: coalesce peer churn instead of building an audible backlog.
		e.run.peerNotification.Store(notification)
	}
	e.opMu.Unlock()
}

func (e *Engine) SetCaptureGain(gain int) error {
	if gain < minimumAudioGain || gain > maximumAudioGain {
		return fmt.Errorf("capture gain must be between %d and %d", minimumAudioGain, maximumAudioGain)
	}
	e.captureGain.Store(int64(gain))
	e.mu.Lock()
	changed := e.state.CaptureGain != gain
	e.state.CaptureGain = gain
	e.mu.Unlock()
	if changed {
		e.publish()
	}
	return nil
}

func (e *Engine) SetPlaybackGain(gain int) error {
	if gain < minimumAudioGain || gain > maximumAudioGain {
		return fmt.Errorf("playback gain must be between %d and %d", minimumAudioGain, maximumAudioGain)
	}
	e.playbackGain.Store(int64(gain))
	e.mu.Lock()
	changed := e.state.PlaybackGain != gain
	e.state.PlaybackGain = gain
	e.mu.Unlock()
	if changed {
		e.publish()
	}
	return nil
}

func (e *Engine) SetRemoteLoudnessNormalization(enabled bool) {
	e.remoteLoudnessNormalization.Store(enabled)
	e.mu.Lock()
	changed := e.state.RemoteLoudnessNormalization != enabled
	e.state.RemoteLoudnessNormalization = enabled
	e.mu.Unlock()
	if changed {
		e.publish()
	}
}

func (e *Engine) SetEchoCancellation(enabled bool) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.echoCancellation.Store(enabled)
	e.mu.Lock()
	changed := e.state.EchoCancellation != enabled
	e.state.EchoCancellation = enabled
	e.mu.Unlock()
	if changed {
		e.publish()
	}
}

func (e *Engine) SetNoiseSuppression(enabled bool) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.noiseSuppression.Store(enabled)
	e.mu.Lock()
	changed := e.state.NoiseSuppression != enabled
	e.state.NoiseSuppression = enabled
	e.mu.Unlock()
	if changed {
		e.publish()
	}
}

func (e *Engine) Start(mediaPort media.AudioPort) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	return e.startLocked(mediaPort)
}

func (e *Engine) startLocked(mediaPort media.AudioPort) error {
	if e.closed {
		return errors.New("audio engine is closed")
	}
	if mediaPort == nil {
		return errors.New("audio media port is required")
	}
	e.mu.RLock()
	if e.state.Running {
		e.mu.RUnlock()
		return nil
	}
	if !e.state.Available {
		e.mu.RUnlock()
		return errors.New("capture and playback devices are required")
	}
	captureID := e.state.CaptureDeviceID
	playbackID := e.state.PlaybackDeviceID
	e.mu.RUnlock()

	encoder, err := newOpusEncoder(e.maxEncodedFrameBytes)
	if err != nil {
		return e.fail(fmt.Errorf("initialise Opus encoder: %w", err))
	}
	processor := newCaptureProcessor()
	captureQueue := newPCMFrameQueue(captureQueueFrames)
	playbackQueue := newPCMFrameQueue(playbackQueueFrames)
	playbackWake := make(chan struct{}, 1)
	captureReady := make(chan struct{}, 1)
	deviceStopped := make(chan struct{}, 1)
	var playbackDemand atomic.Uint64
	initial, _ := playbackQueue.AcquireWrite()
	initial.Index = 0
	clear(initial.Samples[:])
	playbackQueue.CommitWrite()
	playbackDemand.Store(1)
	generation := mediaPort.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	run := &engineRun{cancel: cancel, port: mediaPort, monitorDone: make(chan struct{})}
	run.captureGeneration.Store(generation)

	device, err := e.initDuplexDevice(captureID, playbackID, captureQueue, captureReady, playbackQueue, playbackWake, deviceStopped, &playbackDemand, &run.captureGeneration)
	if err != nil {
		cancel()
		processor.Close()
		return e.fail(err)
	}
	run.device = device
	run.workers.Add(2)
	go e.encodeLoop(ctx, run, captureQueue, captureReady, encoder, processor)
	go e.playbackLoop(ctx, run, playbackQueue, playbackWake, &playbackDemand)
	notifyPlayback(playbackWake)
	if err := device.Start(); err != nil {
		cancel()
		device.Uninit()
		run.workers.Wait()
		mediaPort.Reset()
		return e.fail(fmt.Errorf("start audio device: %w", err))
	}

	e.mu.Lock()
	e.run = run
	run.active.Store(true)
	e.state.Running = true
	e.state.Error = ""
	e.mu.Unlock()
	go e.watchDeviceStop(ctx, run, deviceStopped)
	e.publish()
	return nil
}

func (e *Engine) initDuplexDevice(captureID, playbackID string, captureQueue *pcmFrameQueue, captureReady chan<- struct{}, playbackQueue *pcmFrameQueue, playbackWake chan<- struct{}, stopped chan<- struct{}, playbackDemand, generation *atomic.Uint64) (*malgo.Device, error) {
	config := malgo.DefaultDeviceConfig(malgo.Duplex)
	config.Capture.Format = malgo.FormatF32
	config.Capture.Channels = Channels
	config.Capture.ShareMode = malgo.Shared
	config.Playback.Format = malgo.FormatF32
	config.Playback.Channels = Channels
	config.Playback.ShareMode = malgo.Shared
	config.SampleRate = SampleRate
	config.PeriodSizeInFrames = FrameSamples / 2
	config.Periods = 2
	config.PerformanceProfile = malgo.LowLatency
	config.Alsa.NoMMap = 1
	releaseCapture, err := setDeviceID(e.context.Context, malgo.Capture, captureID, &config.Capture)
	if err != nil {
		return nil, err
	}
	defer releaseCapture()
	releasePlayback, err := setDeviceID(e.context.Context, malgo.Playback, playbackID, &config.Playback)
	if err != nil {
		return nil, err
	}
	defer releasePlayback()
	assembler := captureAssembler{queue: captureQueue, ready: captureReady, generation: generation}
	reader := newPlaybackReader(playbackQueue, playbackWake, playbackDemand, &e.playbackMuted)
	callbacks := malgo.DeviceCallbacks{Data: func(output, input []byte, _ uint32) {
		played := byteFloats(output)
		if len(played) > 0 {
			reader.Read(played)
		}
		if len(input) > 0 {
			assembler.Write(byteFloats(input), played, e.captureMuted.Load())
		}
	}, Stop: func() {
		select {
		case stopped <- struct{}{}:
		default:
		}
	}}
	device, err := malgo.InitDevice(e.context.Context, config, callbacks)
	if err != nil {
		return nil, fmt.Errorf("initialise audio device: %w", err)
	}
	return device, nil
}

func setDeviceID(audioContext malgo.Context, kind malgo.DeviceType, id string, config *malgo.SubConfig) (func(), error) {
	if id == "" {
		return func() {}, nil
	}
	devices, err := audioContext.Devices(kind)
	if err != nil {
		return nil, err
	}
	for index := range devices {
		if devices[index].ID.String() == id {
			config.DeviceID = devices[index].ID.Pointer()
			return func() { freeDevicePointer(config.DeviceID) }, nil
		}
	}
	return nil, errors.New("selected audio device is unavailable")
}

func byteFloats(data []byte) []float32 {
	if len(data) == 0 {
		return nil
	}
	return unsafe.Slice((*float32)(unsafe.Pointer(&data[0])), len(data)/4)
}

func applyGainRamp(samples []float32, from, to float32) float32 {
	if len(samples) == 0 {
		return to
	}
	step := (to - from) / float32(len(samples))
	gain := from
	for index, sample := range samples {
		gain += step
		samples[index] = max(-1, min(1, sample*gain))
	}
	return to
}

func captureMeter(samples []float32) (float64, bool) {
	var energy float64
	clipped := false
	for _, sample := range samples {
		value := float64(sample)
		if math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		energy += value * value
		clipped = clipped || math.Abs(value) >= captureClipThreshold
	}
	if len(samples) == 0 {
		return 0, clipped
	}
	return min(1, math.Sqrt(energy/float64(len(samples)))), clipped
}

type captureMeterWindow struct {
	squaredLevels float64
	frames        int
}

func (w *captureMeterWindow) add(level float64) (float64, bool) {
	w.squaredLevels += level * level
	w.frames++
	if w.frames < captureMeterFrames {
		return 0, false
	}
	level = math.Sqrt(w.squaredLevels / float64(w.frames))
	*w = captureMeterWindow{}
	return level, true
}

func playbackGainFactor(percent int64) float32 {
	// A squared curve adds perceptual range while keeping 100% at unity.
	gain := float32(percent) / 100
	return gain * gain
}

type notificationTone struct {
	phase             float64
	frequency         float64
	secondFrequency   float64
	remaining         int
	audibleWhileMuted bool
}

func (t *notificationTone) start(notification uint32) {
	t.audibleWhileMuted = false
	t.secondFrequency = 0
	switch notification {
	case peerJoinedNotification:
		t.frequency = 520
		t.secondFrequency = 760
	case peerLeftNotification:
		t.frequency = 520
		t.secondFrequency = 340
	case audioUnmutedNotification:
		t.frequency = 880
	case audioMutedNotification:
		t.frequency = 440
	default:
		return
	}
	t.audibleWhileMuted = notification == audioMutedNotification || notification == audioUnmutedNotification
	t.phase = 0
	t.remaining = notificationSamples
}

func (t *notificationTone) mix(samples []float32, playbackMuted bool) bool {
	localOnly := t.remaining > 0 && t.audibleWhileMuted && playbackMuted
	if localOnly {
		clear(samples)
	}
	for index := range samples {
		if t.remaining == 0 {
			return localOnly
		}
		elapsed := notificationSamples - t.remaining
		envelope := min(1, float64(elapsed)/notificationAttack)
		envelope = min(envelope, float64(t.remaining)/notificationRelease)
		level := notificationAmplitude * envelope
		samples[index] = samples[index]*float32(1-level) + float32(math.Sin(t.phase)*level)
		frequency := t.frequency
		if t.secondFrequency != 0 && elapsed >= notificationSamples/2 {
			frequency = t.secondFrequency
		}
		t.phase += 2 * math.Pi * frequency / SampleRate
		t.remaining--
	}
	return localOnly
}

func (e *Engine) encodeLoop(ctx context.Context, run *engineRun, queue *pcmFrameQueue, ready <-chan struct{}, encoder *opusEncoder, processor *captureProcessor) {
	defer run.workers.Done()
	defer processor.Close()
	var encoderGeneration uint64
	var speakingGeneration uint64
	processorGeneration := run.captureGeneration.Load()
	processedEchoCancellation := e.echoCancellation.Load()
	processedNoiseSuppression := e.noiseSuppression.Load()
	var lastProcessedTimestamp uint32
	var localSpeaking speakingHold
	var clipHoldFrames int
	var meterWindow captureMeterWindow
	appliedCaptureGain := float32(e.captureGain.Load()) / 100
	for {
		select {
		case <-ctx.Done():
			return
		case <-ready:
			for {
				frame, ok := queue.AcquireRead()
				if !ok {
					break
				}
				generation := frame.Generation
				if generation != run.captureGeneration.Load() {
					queue.ReleaseRead()
					continue
				}
				if encoderGeneration != generation {
					encoder.codec.Reset()
					encoderGeneration = generation
				}
				if speakingGeneration != generation {
					localSpeaking.reset()
					clipHoldFrames = 0
					meterWindow = captureMeterWindow{}
					e.setLocalSpeaking(run, generation, false)
					speakingGeneration = generation
				}
				echoCancellation := e.echoCancellation.Load()
				noiseSuppression := e.noiseSuppression.Load()
				captureMuted := frame.Muted
				_, inputClipped := captureMeter(frame.Samples[:])
				discontinuous := lastProcessedTimestamp != 0 && frame.Timestamp-lastProcessedTimestamp != FrameSamples
				if processorGeneration != generation || discontinuous {
					processor.ResetEchoCancellation()
					processor.ResetNoiseSuppression()
					processorGeneration = generation
					processedEchoCancellation = echoCancellation
					processedNoiseSuppression = noiseSuppression
				} else {
					if !frame.ReferenceValid || processedEchoCancellation != echoCancellation {
						processor.ResetEchoCancellation()
						processedEchoCancellation = echoCancellation
					}
					if processedNoiseSuppression != noiseSuppression {
						processor.ResetNoiseSuppression()
						processedNoiseSuppression = noiseSuppression
					}
				}
				lastProcessedTimestamp = frame.Timestamp
				if !captureMuted {
					processor.Process(frame.Samples[:], frame.Reference[:], echoCancellation && frame.ReferenceValid, noiseSuppression)
				}
				targetGain := float32(e.captureGain.Load()) / 100
				appliedCaptureGain = applyGainRamp(frame.Samples[:], appliedCaptureGain, targetGain)
				level, outputClipped := captureMeter(frame.Samples[:])
				if captureMuted {
					level = 0
					clipHoldFrames = 0
					meterWindow = captureMeterWindow{}
					e.setCaptureMeter(run, generation, 0, false)
				} else if inputClipped || outputClipped {
					clipHoldFrames = captureClipHoldFrames
				} else if clipHoldFrames > 0 {
					clipHoldFrames--
				}
				if !captureMuted {
					if windowLevel, ready := meterWindow.add(level); ready {
						e.setCaptureMeter(run, generation, windowLevel, clipHoldFrames > 0)
					}
				}
				if run.active.Load() && !captureMuted && localSpeaking.update(level > speakingThreshold) {
					e.setLocalSpeaking(run, generation, localSpeaking.active())
				}
				payload, err := encoder.Encode(frame.Samples[:])
				timestamp := frame.Timestamp
				queue.ReleaseRead()
				if err != nil {
					e.setRuntimeError(fmt.Errorf("encode Opus: %w", err))
					continue
				}
				if len(payload) == 0 || encoder.codec.InDTX() {
					continue
				}
				if generation != run.captureGeneration.Load() {
					encoder.codec.Reset()
					encoderGeneration = 0
					continue
				}
				run.port.SubmitSend(media.SendFrame{
					Timestamp:  timestamp,
					Payload:    payload,
					Deadline:   time.Now().Add(voiceSendBudget),
					Generation: generation,
				})
			}
		}
	}
}

func (e *Engine) watchDeviceStop(ctx context.Context, run *engineRun, stopped <-chan struct{}) {
	defer close(run.monitorDone)
	for waitForDeviceStopCheck(ctx, stopped) {
		e.opMu.Lock()
		if e.closed || ctx.Err() != nil || e.run != run {
			e.opMu.Unlock()
			return
		}
		// WASAPI reports a legacy Stop while miniaudio changes the system default
		// device. Keep watching after a successful native reroute.
		if run.device.IsStarted() {
			e.opMu.Unlock()
			continue
		}
		e.setRuntimeError(errors.New("audio device stopped unexpectedly"))
		e.stopRunLocked(run)
		e.opMu.Unlock()
		return
	}
}

func waitForDeviceStopCheck(ctx context.Context, stopped <-chan struct{}) bool {
	select {
	case <-ctx.Done():
		return false
	case <-stopped:
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(audioRerouteGracePeriod):
		return true
	}
}

// rebuildRunLocked keeps the room media port while rebuilding the native
// duplex stream. Device selection and user audio settings live on Engine.
func (e *Engine) rebuildRunLocked(run *engineRun) error {
	mediaPort := run.port
	e.stopRunLocked(run)
	e.mu.RLock()
	available := e.state.Available
	e.mu.RUnlock()
	if !available {
		return nil
	}
	return e.startLocked(mediaPort)
}

func (e *Engine) playbackLoop(ctx context.Context, run *engineRun, queue *pcmFrameQueue, wake <-chan struct{}, demand *atomic.Uint64) {
	defer run.workers.Done()
	mixer := newMixer(e.maxEncodedFrameBytes)
	nextIndex := uint64(1)
	discard := make([]float32, FrameSamples)
	appliedPlaybackGain := playbackGainFactor(e.playbackGain.Load())
	var notification notificationTone
	mixFrame := func(destination []float32) (bool, error) {
		mixer.setScreenAudioSource(run.port.ScreenAudioSource())
		mixer.loudnessNormalization = e.remoteLoudnessNormalization.Load()
		_, err := mixer.NextInto(destination)
		e.setSpeakingPeerIDs(run, mixer.SpeakingPeerIDs())
		if notification.remaining == 0 {
			queued := run.audioNotification.Swap(0)
			if queued == 0 {
				queued = run.peerNotification.Swap(0)
			}
			if queued != 0 {
				notification.start(queued)
			}
		}
		localOnly := notification.mix(destination, e.playbackMuted.Load())
		targetGain := playbackGainFactor(e.playbackGain.Load())
		appliedPlaybackGain = applyGainRamp(destination, appliedPlaybackGain, targetGain)
		return localOnly, err
	}
	drainPlayback := func() {
		mixer.setScreenAudioSource(run.port.ScreenAudioSource())
		for {
			frame, ok := run.port.TakeReceived()
			if !ok {
				return
			}
			if !frame.ReceivedAt.IsZero() && time.Since(frame.ReceivedAt) > maxPlaybackAge {
				continue
			}
			if err := mixer.Add(frame); err != nil {
				e.setRuntimeError(fmt.Errorf("initialise Opus decoder: %w", err))
			}
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-run.port.ReceivedReady():
			drainPlayback()
		case <-wake:
			drainPlayback()
			target := demand.Load()
			for nextIndex < target {
				_, err := mixFrame(discard)
				if err != nil {
					e.setRuntimeError(fmt.Errorf("decode Opus: %w", err))
				}
				nextIndex++
			}
			for nextIndex <= target {
				slot, ok := queue.AcquireWrite()
				if !ok {
					break
				}
				localOnly, err := mixFrame(slot.Samples[:])
				slot.Index = nextIndex
				slot.LocalOnly = localOnly
				queue.CommitWrite()
				nextIndex++
				if err != nil {
					e.setRuntimeError(fmt.Errorf("decode Opus: %w", err))
				}
			}
		}
	}
}

func (e *Engine) Stop() {
	e.opMu.Lock()
	run := e.run
	e.stopRunLocked(run)
	e.opMu.Unlock()
	if run != nil {
		<-run.monitorDone
	}
}

func (e *Engine) stopRunLocked(run *engineRun) {
	if run == nil || e.run != run {
		return
	}
	e.run = nil
	e.mu.Lock()
	run.active.Store(false)
	e.state.Running = false
	e.state.Speaking = false
	e.state.CaptureLevel = 0
	e.state.CaptureClipped = false
	e.state.SpeakingPeerIDs = []string{}
	e.mu.Unlock()
	run.port.Reset()
	run.cancel()
	if run.device != nil {
		run.device.Uninit()
	}
	run.workers.Wait()
	e.publish()
}

func (e *Engine) Close() error {
	e.opMu.Lock()
	if e.closed {
		e.opMu.Unlock()
		return nil
	}
	run := e.run
	e.stopRunLocked(run)
	e.closed = true
	err := e.context.Uninit()
	e.context.Free()
	e.opMu.Unlock()
	if run != nil {
		<-run.monitorDone
	}
	return err
}

func (e *Engine) fail(err error) error {
	e.mu.Lock()
	e.state.Error = err.Error()
	e.state.Speaking = false
	e.state.SpeakingPeerIDs = []string{}
	e.mu.Unlock()
	e.publish()
	return err
}

func (e *Engine) setRuntimeError(err error) {
	e.mu.Lock()
	if e.state.Error == err.Error() {
		e.mu.Unlock()
		return
	}
	e.state.Error = err.Error()
	e.mu.Unlock()
	e.logger.Warn("voice audio degraded", "error", err)
	e.publish()
}

func (e *Engine) setLocalSpeaking(run *engineRun, generation uint64, speaking bool) {
	e.mu.Lock()
	if !run.active.Load() || e.captureMuted.Load() || generation != run.captureGeneration.Load() || e.state.Speaking == speaking {
		e.mu.Unlock()
		return
	}
	e.state.Speaking = speaking
	e.mu.Unlock()
	e.publish()
}

func (e *Engine) setCaptureMeter(run *engineRun, generation uint64, level float64, clipped bool) {
	e.mu.Lock()
	if !run.active.Load() || generation != run.captureGeneration.Load() {
		e.mu.Unlock()
		return
	}
	if e.captureMuted.Load() {
		level = 0
		clipped = false
	}
	if e.state.CaptureLevel == level && e.state.CaptureClipped == clipped {
		e.mu.Unlock()
		return
	}
	e.state.CaptureLevel = level
	e.state.CaptureClipped = clipped
	e.mu.Unlock()
	e.publish()
}

func (e *Engine) setSpeakingPeerIDs(run *engineRun, peerIDs []string) {
	e.mu.Lock()
	if !run.active.Load() || slices.Equal(e.state.SpeakingPeerIDs, peerIDs) {
		e.mu.Unlock()
		return
	}
	e.state.SpeakingPeerIDs = append(e.state.SpeakingPeerIDs[:0], peerIDs...)
	e.mu.Unlock()
	e.publish()
}

func (e *Engine) publish() {
	select {
	case e.statusChanges <- struct{}{}:
	default:
	}
}

func notifyPlayback(wake chan<- struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

func cloneStatus(state Status) Status {
	state.SpeakingPeerIDs = append([]string{}, state.SpeakingPeerIDs...)
	state.CaptureDevices = append([]Device{}, state.CaptureDevices...)
	state.PlaybackDevices = append([]Device{}, state.PlaybackDevices...)
	return state
}

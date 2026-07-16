package audio

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"bork/internal/media"

	"github.com/gen2brain/malgo"
)

const (
	captureQueueFrames  = 2
	playbackQueueFrames = 2
	voiceSendBudget     = 20 * time.Millisecond
	maxPlaybackAge      = 100 * time.Millisecond
)

type engineRun struct {
	cancel context.CancelFunc
	port   media.AudioPort

	capture           *malgo.Device
	playback          *malgo.Device
	workers           sync.WaitGroup
	captureGeneration atomic.Uint64

	monitorDone chan struct{}
}

type Engine struct {
	logger               *slog.Logger
	context              *malgo.AllocatedContext
	maxEncodedFrameBytes int

	opMu  sync.Mutex
	mu    sync.RWMutex
	state Status

	run           *engineRun
	muted         atomic.Bool
	statusChanges chan struct{}
	closed        bool
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
		statusChanges:        make(chan struct{}, 1),
	}
	if err := engine.refreshDevicesLocked(); err != nil {
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

func (e *Engine) RefreshDevices() (Status, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if e.closed {
		return e.Status(), errors.New("audio engine is closed")
	}
	e.mu.RLock()
	running := e.state.Running
	e.mu.RUnlock()
	if running {
		return e.Status(), errors.New("stop voice before refreshing audio devices")
	}
	if err := e.refreshDevicesLocked(); err != nil {
		return e.Status(), err
	}
	return e.Status(), nil
}

func (e *Engine) refreshDevicesLocked() error {
	capture, err := enumerateDevices(e.context.Context, malgo.Capture)
	if err != nil {
		return fmt.Errorf("enumerate capture devices: %w", err)
	}
	playback, err := enumerateDevices(e.context.Context, malgo.Playback)
	if err != nil {
		return fmt.Errorf("enumerate playback devices: %w", err)
	}
	e.mu.Lock()
	e.state.CaptureDevices = capture
	e.state.PlaybackDevices = playback
	e.state.Available = len(capture) > 0 && len(playback) > 0
	e.state.Error = ""
	if !deviceExists(capture, e.state.CaptureDeviceID) {
		e.state.CaptureDeviceID = ""
	}
	if !deviceExists(playback, e.state.PlaybackDeviceID) {
		e.state.PlaybackDeviceID = ""
	}
	e.mu.Unlock()
	e.publish()
	return nil
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

func (e *Engine) SetDevices(captureID, playbackID string) (Status, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if e.closed {
		return e.Status(), errors.New("audio engine is closed")
	}
	e.mu.Lock()
	if e.state.Running {
		e.mu.Unlock()
		return e.Status(), errors.New("stop voice before changing audio devices")
	}
	if !deviceExists(e.state.CaptureDevices, captureID) {
		e.mu.Unlock()
		return e.Status(), errors.New("capture device is unavailable")
	}
	if !deviceExists(e.state.PlaybackDevices, playbackID) {
		e.mu.Unlock()
		return e.Status(), errors.New("playback device is unavailable")
	}
	e.state.CaptureDeviceID = captureID
	e.state.PlaybackDeviceID = playbackID
	state := cloneStatus(e.state)
	e.mu.Unlock()
	e.publish()
	return state, nil
}

func (e *Engine) SetMuted(muted bool) Status {
	e.opMu.Lock()
	if muted {
		e.muted.Store(true)
	}
	if e.run != nil {
		generation := e.run.port.InvalidateSend()
		e.run.captureGeneration.Store(generation)
	}
	if !muted {
		e.muted.Store(false)
	}
	e.mu.Lock()
	e.state.Muted = muted
	state := cloneStatus(e.state)
	e.mu.Unlock()
	e.publish()
	e.opMu.Unlock()
	return state
}

func (e *Engine) Start(mediaPort media.AudioPort) (Status, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if e.closed {
		return e.Status(), errors.New("audio engine is closed")
	}
	if mediaPort == nil {
		return e.Status(), errors.New("audio media port is required")
	}
	e.mu.RLock()
	if e.state.Running {
		state := cloneStatus(e.state)
		e.mu.RUnlock()
		return state, nil
	}
	if !e.state.Available {
		e.mu.RUnlock()
		return e.Status(), errors.New("capture and playback devices are required")
	}
	captureID := e.state.CaptureDeviceID
	playbackID := e.state.PlaybackDeviceID
	e.mu.RUnlock()

	encoder, err := newOpusEncoder(e.maxEncodedFrameBytes)
	if err != nil {
		return e.fail(fmt.Errorf("initialise Opus encoder: %w", err))
	}
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

	capture, err := e.initCaptureDevice(captureID, captureQueue, captureReady, deviceStopped, &run.captureGeneration)
	if err != nil {
		cancel()
		return e.fail(err)
	}
	playback, err := e.initPlaybackDevice(playbackID, playbackQueue, playbackWake, deviceStopped, &playbackDemand)
	if err != nil {
		cancel()
		capture.Uninit()
		return e.fail(err)
	}
	run.capture = capture
	run.playback = playback
	run.workers.Add(2)
	go e.encodeLoop(ctx, run, captureQueue, captureReady, encoder)
	go e.playbackLoop(ctx, run, playbackQueue, playbackWake, &playbackDemand)
	notifyPlayback(playbackWake)
	if err := playback.Start(); err != nil {
		cancel()
		capture.Uninit()
		playback.Uninit()
		run.workers.Wait()
		mediaPort.Reset()
		return e.fail(fmt.Errorf("start playback device: %w", err))
	}
	if err := capture.Start(); err != nil {
		cancel()
		capture.Uninit()
		playback.Uninit()
		run.workers.Wait()
		mediaPort.Reset()
		return e.fail(fmt.Errorf("start capture device: %w", err))
	}

	e.run = run
	go e.watchDeviceStop(ctx, run, deviceStopped)

	e.mu.Lock()
	e.state.Running = true
	e.state.Error = ""
	state := cloneStatus(e.state)
	e.mu.Unlock()
	e.publish()
	return state, nil
}

func (e *Engine) initCaptureDevice(deviceID string, queue *pcmFrameQueue, ready, stopped chan<- struct{}, generation *atomic.Uint64) (*malgo.Device, error) {
	config := malgo.DefaultDeviceConfig(malgo.Capture)
	config.Capture.Format = malgo.FormatF32
	config.Capture.Channels = Channels
	config.Capture.ShareMode = malgo.Shared
	config.SampleRate = SampleRate
	config.PeriodSizeInFrames = FrameSamples / 2
	config.Periods = 2
	config.PerformanceProfile = malgo.LowLatency
	config.Alsa.NoMMap = 1
	release, err := setDeviceID(e.context.Context, malgo.Capture, deviceID, &config.Capture)
	if err != nil {
		return nil, err
	}
	defer release()
	assembler := captureAssembler{queue: queue, ready: ready, generation: generation}
	callbacks := malgo.DeviceCallbacks{Data: func(_ []byte, input []byte, _ uint32) {
		if len(input) == 0 {
			return
		}
		assembler.Write(byteFloats(input), e.muted.Load())
	}, Stop: func() {
		select {
		case stopped <- struct{}{}:
		default:
		}
	}}
	device, err := malgo.InitDevice(e.context.Context, config, callbacks)
	if err != nil {
		return nil, fmt.Errorf("initialise capture device: %w", err)
	}
	return device, nil
}

func (e *Engine) initPlaybackDevice(deviceID string, queue *pcmFrameQueue, wake, stopped chan<- struct{}, demand *atomic.Uint64) (*malgo.Device, error) {
	config := malgo.DefaultDeviceConfig(malgo.Playback)
	config.Playback.Format = malgo.FormatF32
	config.Playback.Channels = Channels
	config.Playback.ShareMode = malgo.Shared
	config.SampleRate = SampleRate
	config.PeriodSizeInFrames = FrameSamples / 2
	config.Periods = 2
	config.PerformanceProfile = malgo.LowLatency
	config.Alsa.NoMMap = 1
	release, err := setDeviceID(e.context.Context, malgo.Playback, deviceID, &config.Playback)
	if err != nil {
		return nil, err
	}
	defer release()
	reader := newPlaybackReader(queue, wake, demand)
	callbacks := malgo.DeviceCallbacks{Data: func(output, _ []byte, _ uint32) {
		if len(output) == 0 {
			return
		}
		reader.Read(byteFloats(output))
	}, Stop: func() {
		select {
		case stopped <- struct{}{}:
		default:
		}
	}}
	device, err := malgo.InitDevice(e.context.Context, config, callbacks)
	if err != nil {
		return nil, fmt.Errorf("initialise playback device: %w", err)
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

func (e *Engine) encodeLoop(ctx context.Context, run *engineRun, queue *pcmFrameQueue, ready <-chan struct{}, encoder *opusEncoder) {
	defer run.workers.Done()
	var encoderGeneration uint64
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
					encoder.Reset()
					encoderGeneration = generation
				}
				payload, err := encoder.Encode(frame.Samples[:])
				timestamp := frame.Timestamp
				queue.ReleaseRead()
				if err != nil {
					e.setRuntimeError(fmt.Errorf("encode Opus: %w", err))
					continue
				}
				if len(payload) == 0 {
					continue
				}
				if generation != run.captureGeneration.Load() {
					encoder.Reset()
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
	select {
	case <-ctx.Done():
		return
	case <-stopped:
		if ctx.Err() != nil {
			return
		}
	}
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if e.closed || ctx.Err() != nil || e.run != run {
		return
	}
	e.setRuntimeError(errors.New("audio device stopped unexpectedly"))
	e.stopRunLocked(run)
}

func (e *Engine) playbackLoop(ctx context.Context, run *engineRun, queue *pcmFrameQueue, wake <-chan struct{}, demand *atomic.Uint64) {
	defer run.workers.Done()
	mixer := newMixer(e.maxEncodedFrameBytes)
	nextIndex := uint64(1)
	discard := make([]float32, FrameSamples)
	drainPlayback := func() {
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
				_, err := mixer.NextInto(discard)
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
				_, err := mixer.NextInto(slot.Samples[:])
				slot.Index = nextIndex
				queue.CommitWrite()
				nextIndex++
				if err != nil {
					e.setRuntimeError(fmt.Errorf("decode Opus: %w", err))
				}
			}
		}
	}
}

func (e *Engine) Stop() Status {
	e.opMu.Lock()
	run := e.run
	state := e.stopRunLocked(run)
	e.opMu.Unlock()
	if run != nil {
		<-run.monitorDone
	}
	return state
}

func (e *Engine) stopRunLocked(run *engineRun) Status {
	if run == nil || e.run != run {
		return e.Status()
	}
	e.run = nil
	e.mu.Lock()
	e.state.Running = false
	state := cloneStatus(e.state)
	e.mu.Unlock()
	run.port.Reset()
	run.cancel()
	if run.capture != nil {
		run.capture.Uninit()
	}
	if run.playback != nil {
		run.playback.Uninit()
	}
	run.workers.Wait()
	e.publish()
	return state
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

func (e *Engine) fail(err error) (Status, error) {
	e.mu.Lock()
	e.state.Error = err.Error()
	state := cloneStatus(e.state)
	e.mu.Unlock()
	e.publish()
	return state, err
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
	state.CaptureDevices = append([]Device{}, state.CaptureDevices...)
	state.PlaybackDevices = append([]Device{}, state.PlaybackDevices...)
	return state
}

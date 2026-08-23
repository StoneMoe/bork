//go:build windows && cgo

package screenshare

/*
#cgo windows LDFLAGS: -lmmdevapi -lole32 -luuid
#include <stdint.h>

typedef struct bork_screen_audio_capture bork_screen_audio_capture;

bork_screen_audio_capture *bork_screen_audio_capture_start(int32_t *result_out);
int32_t bork_screen_audio_capture_read(
    bork_screen_audio_capture *capture, float *output, uint32_t frame_count);
int32_t bork_screen_audio_capture_stop(bork_screen_audio_capture *capture);
void bork_screen_audio_capture_destroy(bork_screen_audio_capture *capture);
*/
import "C"

import (
	"fmt"
	"io"
	"runtime"
	"sync"
	"unsafe"
)

type audioSource struct {
	mu      sync.Mutex
	capture *C.bork_screen_audio_capture
	frames  chan audioPCMFrame
	done    chan struct{}
	readErr error
}

func startAudioSource() (*audioSource, error) {
	source := &audioSource{
		frames: make(chan audioPCMFrame, 1),
		done:   make(chan struct{}),
	}
	ready := make(chan error, 1)
	go source.run(ready)
	if err := <-ready; err != nil {
		return nil, err
	}
	return source, nil
}

func (s *audioSource) run(ready chan<- error) {
	// COM interfaces stay on one OS thread for their whole lifetime. Close only
	// signals a Win32 event, which is safe from another thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(s.done)
	defer close(s.frames)

	var result C.int32_t
	capture := C.bork_screen_audio_capture_start(&result)
	if capture == nil {
		ready <- audioError("start Windows screen audio capture", result)
		return
	}
	s.mu.Lock()
	s.capture = capture
	s.mu.Unlock()
	ready <- nil

	var timestamp uint32
	for {
		var frame audioPCMFrame
		result = C.bork_screen_audio_capture_read(
			capture,
			(*C.float)(unsafe.Pointer(&frame.Samples[0])),
			C.uint32_t(audioFrameSamples),
		)
		if result == C.int32_t(1) {
			break
		}
		if result < 0 {
			s.mu.Lock()
			s.readErr = audioError("read Windows screen audio", result)
			s.mu.Unlock()
			break
		}
		frame.Timestamp = timestamp
		timestamp += audioFrameSamples
		s.deliver(frame)
	}

	s.mu.Lock()
	s.capture = nil
	s.mu.Unlock()
	C.bork_screen_audio_capture_destroy(capture)
}

func (s *audioSource) deliver(frame audioPCMFrame) {
	for {
		select {
		case s.frames <- frame:
			return
		case <-s.frames:
			// Audio is live: drop an unread frame instead of building latency.
		}
	}
}

func (s *audioSource) readFrame() (audioPCMFrame, error) {
	frame, ok := <-s.frames
	if ok {
		return frame, nil
	}
	s.mu.Lock()
	err := s.readErr
	s.mu.Unlock()
	if err != nil {
		return audioPCMFrame{}, err
	}
	return audioPCMFrame{}, io.EOF
}

func (s *audioSource) close() error {
	s.mu.Lock()
	var stopErr error
	if s.capture != nil {
		result := C.bork_screen_audio_capture_stop(s.capture)
		if result < 0 {
			stopErr = audioError("stop Windows screen audio capture", result)
		}
	}
	s.mu.Unlock()
	<-s.done
	return stopErr
}

func audioError(operation string, result C.int32_t) error {
	return fmt.Errorf("%s: HRESULT 0x%08x", operation, uint32(result))
}

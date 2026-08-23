//go:build windows && cgo

package screenshare

/*
#cgo windows CXXFLAGS: -std=c++17
#cgo windows LDFLAGS: -ld3d11 -ldxgi -lmfplat -lmf -lmfuuid -lwmcodecdspuuid -lole32 -loleaut32 -lruntimeobject -luuid
#include "video_capture_windows.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unsafe"
)

const hresultScreenSourceClosed = 0x80070026

var errScreenSourceClosed = errors.New("screen capture source closed")

type videoSource struct {
	mu      sync.Mutex
	capture *C.bork_screen_video_capture
	frames  chan VideoFrame
	done    chan struct{}
	readErr error
}

type videoStartResult struct {
	info VideoInfo
	err  error
}

func startVideoSource(sourceID string, maxFrameBytes int) (*videoSource, VideoInfo, error) {
	kind, handle, err := parseVideoSource(sourceID)
	if err != nil {
		return nil, VideoInfo{}, err
	}

	source := &videoSource{
		frames: make(chan VideoFrame, 1),
		done:   make(chan struct{}),
	}
	ready := make(chan videoStartResult, 1)
	go source.run(kind, handle, maxFrameBytes, ready)
	result := <-ready
	if result.err != nil {
		return nil, VideoInfo{}, result.err
	}
	return source, result.info, nil
}

func parseVideoSource(sourceID string) (C.int32_t, uintptr, error) {
	kind, value, ok := strings.Cut(sourceID, ":")
	if !ok || value == "" {
		return 0, 0, fmt.Errorf("invalid screen capture source %q", sourceID)
	}
	handle, err := strconv.ParseUint(value, 16, strconv.IntSize)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid screen capture source %q", sourceID)
	}
	switch kind {
	case SourceMonitor:
		return C.BORK_SCREEN_VIDEO_SOURCE_MONITOR, uintptr(handle), nil
	case SourceWindow:
		return C.BORK_SCREEN_VIDEO_SOURCE_WINDOW, uintptr(handle), nil
	default:
		return 0, 0, fmt.Errorf("invalid screen capture source %q", sourceID)
	}
}

func (s *videoSource) run(kind C.int32_t, handle uintptr, maxFrameBytes int, ready chan<- videoStartResult) {
	// Windows Graphics Capture, D3D, and Media Foundation objects are created,
	// used, and destroyed on this one OS thread. Close only signals native work.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(s.done)
	defer close(s.frames)

	var nativeInfo C.bork_screen_video_info
	var result C.int32_t
	capture := C.bork_screen_video_capture_start(
		kind,
		C.uintptr_t(handle),
		C.uint32_t(maxFrameBytes),
		&nativeInfo,
		&result,
	)
	if capture == nil {
		ready <- videoStartResult{err: videoError("start Windows screen capture", result)}
		return
	}
	codec, err := videoCodec(nativeInfo.codec)
	if err != nil {
		C.bork_screen_video_capture_destroy(capture)
		ready <- videoStartResult{err: err}
		return
	}

	s.mu.Lock()
	s.capture = capture
	s.mu.Unlock()
	ready <- videoStartResult{info: VideoInfo{Codec: codec, Width: int(nativeInfo.width), Height: int(nativeInfo.height)}}

	s.readLoop(capture)
	s.mu.Lock()
	s.capture = nil
	s.mu.Unlock()
	C.bork_screen_video_capture_destroy(capture)
}

func videoCodec(codec C.int32_t) (string, error) {
	switch codec {
	case C.BORK_SCREEN_VIDEO_CODEC_H264_BASELINE:
		return VideoCodecH264Baseline, nil
	case C.BORK_SCREEN_VIDEO_CODEC_H264_MAIN:
		return VideoCodecH264Main, nil
	default:
		return "", fmt.Errorf("Windows screen encoder returned unknown H.264 profile %d", int32(codec))
	}
}

func (s *videoSource) readLoop(capture *C.bork_screen_video_capture) {
	for {
		frame, stopped, err := readNativeVideoFrame(capture)
		if stopped {
			return
		}
		if err != nil {
			s.mu.Lock()
			s.readErr = err
			s.mu.Unlock()
			return
		}
		s.deliver(capture, frame)
	}
}

func readNativeVideoFrame(capture *C.bork_screen_video_capture) (VideoFrame, bool, error) {
	var frame C.bork_screen_video_frame
	result := C.bork_screen_video_capture_read(capture, &frame)
	if result == 1 { // S_FALSE means Close stopped the native reader.
		return VideoFrame{}, true, nil
	}
	if uint32(result) == hresultScreenSourceClosed {
		return VideoFrame{}, false, errScreenSourceClosed
	}
	if result < 0 {
		return VideoFrame{}, false, videoError("read Windows screen capture", result)
	}
	return VideoFrame{
		Timestamp: uint64(frame.timestamp_us),
		Duration:  uint32(frame.duration_us),
		KeyFrame:  frame.key_frame != 0,
		Payload:   C.GoBytes(unsafe.Pointer(frame.data), C.int(frame.length)),
	}, false, nil
}

func (s *videoSource) deliver(capture *C.bork_screen_video_capture, frame VideoFrame) {
	for {
		select {
		case s.frames <- frame:
			return
		case <-s.frames:
			// Replacing an unread encoded frame breaks the prediction chain.
			// Ask the encoder for a key frame so the receiver can recover.
			C.bork_screen_video_capture_force_key_frame(capture)
		}
	}
}

func (s *videoSource) readFrame() (VideoFrame, error) {
	frame, ok := <-s.frames
	if ok {
		return frame, nil
	}
	s.mu.Lock()
	err := s.readErr
	s.mu.Unlock()
	if err != nil {
		return VideoFrame{}, err
	}
	return VideoFrame{}, io.EOF
}

func (s *videoSource) forceKeyFrame() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.capture == nil {
		if s.readErr != nil {
			return s.readErr
		}
		return io.EOF
	}
	result := C.bork_screen_video_capture_force_key_frame(s.capture)
	if result < 0 {
		return videoError("request Windows screen key frame", result)
	}
	return nil
}

func (s *videoSource) close() error {
	s.mu.Lock()
	var stopErr error
	if s.capture != nil {
		result := C.bork_screen_video_capture_stop(s.capture)
		if result < 0 {
			stopErr = videoError("stop Windows screen capture", result)
		}
	}
	s.mu.Unlock()
	<-s.done
	return stopErr
}

func videoError(operation string, result C.int32_t) error {
	return fmt.Errorf("%s: HRESULT 0x%08x", operation, uint32(result))
}

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
	mu       sync.Mutex
	capture  *C.bork_screen_video_capture
	frames   chan VideoFrame
	done     chan struct{}
	readErr  error
	stopping bool
}

type videoStartResult struct {
	info VideoInfo
	err  error
}

func startVideoSource(sourceID string, maxFrameBytes, maxWidth, maxHeight int) (*videoSource, VideoInfo, error) {
	kind, handle, err := parseVideoSource(sourceID)
	if err != nil {
		return nil, VideoInfo{}, err
	}

	source := &videoSource{
		frames: make(chan VideoFrame, 1),
		done:   make(chan struct{}),
	}
	ready := make(chan videoStartResult, 1)
	go source.run(kind, handle, maxFrameBytes, maxWidth, maxHeight, ready)
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

func (s *videoSource) run(
	kind C.int32_t,
	handle uintptr,
	maxFrameBytes, maxWidth, maxHeight int,
	ready chan<- videoStartResult,
) {
	// Windows Graphics Capture, D3D, and Media Foundation objects are created,
	// used, and destroyed on this one OS thread. Close only signals native work.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(s.done)
	defer close(s.frames)

	capture, info, err := startNativeVideoCapture(
		kind, handle, maxFrameBytes, maxWidth, maxHeight, false,
	)
	if err != nil {
		ready <- videoStartResult{err: err}
		return
	}
	s.installCapture(capture)
	ready <- videoStartResult{info: info}

	reconfigure := s.readCapture(capture, info)
	stopping := s.destroyCapture(capture)
	if !reconfigure || stopping {
		return
	}

	capture, info, err = startNativeVideoCapture(
		kind, handle, maxFrameBytes, maxWidth, maxHeight, true,
	)
	if err != nil {
		s.setReadError(err)
		return
	}
	if !s.installCapture(capture) {
		C.bork_screen_video_capture_destroy(capture)
		return
	}
	s.readCapture(capture, info)
	s.destroyCapture(capture)
}

func startNativeVideoCapture(
	kind C.int32_t,
	handle uintptr,
	maxFrameBytes, maxWidth, maxHeight int,
	fullOutput bool,
) (*C.bork_screen_video_capture, VideoInfo, error) {
	var nativeInfo C.bork_screen_video_info
	var result C.int32_t
	var full C.int32_t
	if fullOutput {
		full = 1
	}
	capture := C.bork_screen_video_capture_start(
		kind,
		C.uintptr_t(handle),
		C.uint32_t(maxFrameBytes),
		C.uint32_t(maxWidth),
		C.uint32_t(maxHeight),
		full,
		&nativeInfo,
		&result,
	)
	if capture == nil {
		return nil, VideoInfo{}, videoError("start Windows screen capture", result)
	}
	codec, err := videoCodec(nativeInfo.codec)
	if err != nil {
		C.bork_screen_video_capture_destroy(capture)
		return nil, VideoInfo{}, err
	}
	return capture, VideoInfo{
		Codec: codec, Width: int(nativeInfo.width), Height: int(nativeInfo.height),
	}, nil
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

type nativeVideoReadStatus uint8

const (
	nativeVideoReadFrame nativeVideoReadStatus = iota
	nativeVideoReadStopped
	nativeVideoReadReconfigure
)

func (s *videoSource) readCapture(capture *C.bork_screen_video_capture, info VideoInfo) bool {
	reconfigure, err := s.readLoop(capture, info)
	if err != nil {
		s.setReadError(err)
	}
	return reconfigure
}

func (s *videoSource) readLoop(capture *C.bork_screen_video_capture, info VideoInfo) (bool, error) {
	for {
		frame, status, err := readNativeVideoFrame(capture, info)
		if err != nil {
			return false, err
		}
		switch status {
		case nativeVideoReadStopped:
			return false, nil
		case nativeVideoReadReconfigure:
			return true, nil
		}
		s.deliver(capture, frame)
	}
}

func readNativeVideoFrame(
	capture *C.bork_screen_video_capture,
	info VideoInfo,
) (VideoFrame, nativeVideoReadStatus, error) {
	var frame C.bork_screen_video_frame
	result := C.bork_screen_video_capture_read(capture, &frame)
	if result == 1 { // S_FALSE means Close stopped the native reader.
		return VideoFrame{}, nativeVideoReadStopped, nil
	}
	if result == C.BORK_SCREEN_VIDEO_READ_RECONFIGURE {
		return VideoFrame{}, nativeVideoReadReconfigure, nil
	}
	if uint32(result) == hresultScreenSourceClosed {
		return VideoFrame{}, nativeVideoReadStopped, errScreenSourceClosed
	}
	if result < 0 {
		return VideoFrame{}, nativeVideoReadStopped, videoError("read Windows screen capture", result)
	}
	return VideoFrame{
		Info:          info,
		DisplayWidth:  int(frame.display_width),
		DisplayHeight: int(frame.display_height),
		Timestamp:     uint64(frame.timestamp_us),
		Duration:      uint32(frame.duration_us),
		KeyFrame:      frame.key_frame != 0,
		Payload:       C.GoBytes(unsafe.Pointer(frame.data), C.int(frame.length)),
	}, nativeVideoReadFrame, nil
}

func (s *videoSource) installCapture(capture *C.bork_screen_video_capture) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return false
	}
	s.capture = capture
	return true
}

func (s *videoSource) destroyCapture(capture *C.bork_screen_video_capture) bool {
	s.mu.Lock()
	if s.capture == capture {
		s.capture = nil
	}
	stopping := s.stopping
	s.mu.Unlock()
	C.bork_screen_video_capture_destroy(capture)
	return stopping
}

func (s *videoSource) setReadError(err error) {
	s.mu.Lock()
	s.readErr = err
	s.mu.Unlock()
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
	s.stopping = true
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

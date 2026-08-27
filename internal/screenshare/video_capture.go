package screenshare

import (
	"fmt"
)

const (
	VideoCodecH264Baseline = "avc1.42E032"
	VideoCodecH264Main     = "avc1.4D4032"
)

// VideoInfo describes the H.264 stream containing a frame. It changes once if
// a shared window grows beyond its initial coded size.
type VideoInfo struct {
	Codec  string
	Width  int
	Height int
}

// VideoFrame is one H.264 Annex-B access unit. DisplayWidth and DisplayHeight
// describe the centered content area inside the fixed coded frame. Timestamp
// and Duration use microseconds, matching the screen-video wire protocol and
// WebCodecs.
type VideoFrame struct {
	Info          VideoInfo
	DisplayWidth  int
	DisplayHeight int
	Timestamp     uint64
	Duration      uint32
	KeyFrame      bool
	Payload       []byte
}

// VideoCapture captures, tone maps, resizes, and encodes one native source.
// ReadFrame must be called by one goroutine; Close may be called concurrently.
type VideoCapture struct {
	source *videoSource
	info   VideoInfo
}

func StartVideoCapture(sourceID string, maxFrameBytes, maxWidth, maxHeight int) (*VideoCapture, error) {
	if maxFrameBytes <= 0 {
		return nil, fmt.Errorf("screen video frame limit must be positive")
	}
	if maxWidth < 2 || maxHeight < 2 {
		return nil, fmt.Errorf("screen video dimensions must be at least 2x2")
	}
	source, info, err := startVideoSource(sourceID, maxFrameBytes, maxWidth, maxHeight)
	if err != nil {
		return nil, err
	}
	return &VideoCapture{source: source, info: info}, nil
}

// Info returns the initial stream configuration. ReadFrame carries the active
// configuration in case a growing window promotes to the full coded size.
func (c *VideoCapture) Info() VideoInfo {
	return c.info
}

func (c *VideoCapture) ReadFrame() (VideoFrame, error) {
	return c.source.readFrame()
}

// ForceKeyFrame makes the next accepted capture frame independently
// decodable. Call it after a frame is dropped before transport.
func (c *VideoCapture) ForceKeyFrame() error {
	return c.source.forceKeyFrame()
}

func (c *VideoCapture) Close() error {
	return c.source.close()
}

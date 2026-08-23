package screenshare

import (
	"fmt"
)

const (
	VideoCodecH264Baseline = "avc1.42E01F"
	VideoCodecH264Main     = "avc1.4D401F"
)

// VideoInfo is fixed for one capture. A window may resize while sharing, but
// frames keep this encoded size so receivers do not need a second start event.
type VideoInfo struct {
	Codec  string
	Width  int
	Height int
}

// VideoFrame is one H.264 Annex-B access unit. Timestamp and Duration use
// microseconds, matching the screen-video wire protocol and WebCodecs.
type VideoFrame struct {
	Timestamp uint64
	Duration  uint32
	KeyFrame  bool
	Payload   []byte
}

// VideoCapture captures, tone maps, resizes, and encodes one native source.
// ReadFrame must be called by one goroutine; Close may be called concurrently.
type VideoCapture struct {
	source *videoSource
	info   VideoInfo
}

func StartVideoCapture(sourceID string, maxFrameBytes int) (*VideoCapture, error) {
	if maxFrameBytes <= 0 {
		return nil, fmt.Errorf("screen video frame limit must be positive")
	}
	source, info, err := startVideoSource(sourceID, maxFrameBytes)
	if err != nil {
		return nil, err
	}
	return &VideoCapture{source: source, info: info}, nil
}

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

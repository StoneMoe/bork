package audio

const (
	SampleRate       = 48000
	Channels         = 1
	FrameDuration    = 10
	FrameSamples     = SampleRate * FrameDuration / 1000
	prebufferFrames  = 2
	maxJitterFrames  = 16
	streamIdleFrames = 100
	maxPLCFrames     = 10
)

type Device struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsDefault bool   `json:"isDefault"`
}

type Status struct {
	Available        bool     `json:"available"`
	Running          bool     `json:"running"`
	Muted            bool     `json:"muted"`
	CaptureDeviceID  string   `json:"captureDeviceId"`
	PlaybackDeviceID string   `json:"playbackDeviceId"`
	CaptureDevices   []Device `json:"captureDevices"`
	PlaybackDevices  []Device `json:"playbackDevices"`
	Error            string   `json:"error,omitempty"`
}

type Options struct {
	MaxEncodedFrameBytes int
}

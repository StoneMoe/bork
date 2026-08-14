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

	speakingThreshold     = 0.01
	speakingReleaseFrames = 300 / FrameDuration
	captureClipThreshold  = 0.999
	captureClipHoldFrames = 1000 / FrameDuration
	captureMeterFrames    = 50 / FrameDuration

	defaultAudioGain = 100
	minimumAudioGain = 0
	maximumAudioGain = 200

	normalizationTargetRMS       = 0.10
	normalizationMinimumGain     = 0.25
	normalizationMaximumGain     = 4.0
	normalizationAttackFrames    = 50 / FrameDuration
	normalizationReleaseFrames   = 500 / FrameDuration
	normalizationSmoothingFrames = 100 / FrameDuration
	normalizationHoldFrames      = 300 / FrameDuration
)

type Device struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsDefault bool   `json:"isDefault"`
}

type Status struct {
	Available                   bool     `json:"available"`
	Running                     bool     `json:"running"`
	CaptureMuted                bool     `json:"captureMuted"`
	PlaybackMuted               bool     `json:"playbackMuted"`
	CaptureGain                 int      `json:"captureGain"`
	CaptureLevel                float64  `json:"captureLevel"`
	CaptureClipped              bool     `json:"captureClipped"`
	PlaybackGain                int      `json:"playbackGain"`
	EchoCancellation            bool     `json:"echoCancellation"`
	NoiseSuppression            bool     `json:"noiseSuppression"`
	RemoteLoudnessNormalization bool     `json:"remoteLoudnessNormalization"`
	Speaking                    bool     `json:"speaking"`
	SpeakingPeerIDs             []string `json:"speakingPeerIds"`
	CaptureDeviceID             string   `json:"captureDeviceId"`
	PlaybackDeviceID            string   `json:"playbackDeviceId"`
	CaptureDevices              []Device `json:"captureDevices"`
	PlaybackDevices             []Device `json:"playbackDevices"`
	Error                       string   `json:"error,omitempty"`
}

func defaultStatus() Status {
	return Status{
		CaptureGain:                 defaultAudioGain,
		PlaybackGain:                defaultAudioGain,
		EchoCancellation:            true,
		NoiseSuppression:            true,
		RemoteLoudnessNormalization: true,
		SpeakingPeerIDs:             []string{},
	}
}

type Options struct {
	MaxEncodedFrameBytes int
}

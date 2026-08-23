package screenshare

import (
	"errors"
	"fmt"

	"github.com/thesyncim/gopus"
)

const (
	audioSampleRate   = 48000
	audioChannels     = 1
	audioFrameSamples = 480
	audioBitrate      = 48000
)

var ErrUnsupported = errors.New("screen sharing is not supported on this platform")

// AudioFrame is one 10 ms raw Opus packet. Timestamp uses the same
// 48 kHz sample clock as microphone audio.
type AudioFrame struct {
	Timestamp uint32
	Payload   []byte
}

type audioPCMFrame struct {
	Timestamp uint32
	Samples   [audioFrameSamples]float32
}

// AudioCapture captures system output without playing it locally.
// ReadFrame must be called by one goroutine; Close may be called concurrently.
type AudioCapture struct {
	source         *audioSource
	encoder        *gopus.Encoder
	maxPacketBytes int
}

func StartAudioCapture(maxPacketBytes int) (*AudioCapture, error) {
	encoder, err := gopus.NewEncoder(gopus.EncoderConfig{
		SampleRate:  audioSampleRate,
		Channels:    audioChannels,
		Application: gopus.ApplicationAudio,
	})
	if err != nil {
		return nil, err
	}
	for _, setting := range []struct {
		name string
		set  func() error
	}{
		{"frame size", func() error { return encoder.SetFrameSize(audioFrameSamples) }},
		{"bitrate", func() error { return encoder.SetBitrate(audioBitrate) }},
		{"complexity", func() error { return encoder.SetComplexity(5) }},
		{"bitrate mode", func() error { return encoder.SetBitrateMode(gopus.BitrateModeCVBR) }},
		{"in-band FEC", func() error { return encoder.SetInBandFEC(gopus.InBandFECEnabled) }},
		{"packet loss", func() error { return encoder.SetPacketLoss(5) }},
	} {
		if err := setting.set(); err != nil {
			return nil, fmt.Errorf("configure screen audio Opus %s: %w", setting.name, err)
		}
	}
	encoder.SetDTX(true)

	source, err := startAudioSource()
	if err != nil {
		return nil, err
	}
	return &AudioCapture{source: source, encoder: encoder, maxPacketBytes: maxPacketBytes}, nil
}

func (c *AudioCapture) ReadFrame() (AudioFrame, error) {
	for {
		frame, err := c.source.readFrame()
		if err != nil {
			return AudioFrame{}, err
		}
		payload := make([]byte, c.maxPacketBytes)
		count, err := c.encoder.Encode(frame.Samples[:], payload)
		if err != nil {
			return AudioFrame{}, fmt.Errorf("encode screen audio: %w", err)
		}
		if count == 0 || c.encoder.InDTX() {
			continue
		}
		return AudioFrame{Timestamp: frame.Timestamp, Payload: payload[:count:count]}, nil
	}
}

func (c *AudioCapture) Close() error {
	return c.source.close()
}

package audio

import (
	"fmt"

	"github.com/thesyncim/gopus"
)

const (
	defaultVoiceBitrate = 24000
	minimumVoiceBitrate = 12000
)

type opusEncoder struct {
	codec         *gopus.Encoder
	maxFrameBytes int
}

func newOpusEncoder(maxFrameBytes int) (*opusEncoder, error) {
	codec, err := gopus.NewEncoder(gopus.EncoderConfig{
		SampleRate:  SampleRate,
		Channels:    Channels,
		Application: gopus.ApplicationVoIP,
	})
	if err != nil {
		return nil, err
	}
	for _, setting := range []struct {
		name string
		set  func() error
	}{
		{"frame size", func() error { return codec.SetFrameSize(FrameSamples) }},
		{"bitrate", func() error { return codec.SetBitrate(defaultVoiceBitrate) }},
		{"complexity", func() error { return codec.SetComplexity(5) }},
		{"bitrate mode", func() error { return codec.SetBitrateMode(gopus.BitrateModeCVBR) }},
		{"in-band FEC", func() error { return codec.SetInBandFEC(gopus.InBandFECEnabled) }},
		{"packet loss", func() error { return codec.SetPacketLoss(5) }},
	} {
		if err := setting.set(); err != nil {
			return nil, fmt.Errorf("configure Opus %s: %w", setting.name, err)
		}
	}
	codec.SetDTX(true)
	return &opusEncoder{codec: codec, maxFrameBytes: maxFrameBytes}, nil
}

func (e *opusEncoder) Encode(pcm []float32) ([]byte, error) {
	payload := make([]byte, e.maxFrameBytes)
	count, err := e.codec.Encode(pcm, payload)
	if err != nil {
		return nil, err
	}
	return payload[:count:count], nil
}

func newOpusDecoder(maxFrameBytes int) (*gopus.Decoder, error) {
	config := gopus.DefaultDecoderConfig(SampleRate, Channels)
	config.MaxPacketSamples = FrameSamples
	config.MaxPacketBytes = maxFrameBytes
	return gopus.NewDecoder(config)
}

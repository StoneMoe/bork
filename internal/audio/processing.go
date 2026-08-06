package audio

import (
	"math"

	"github.com/ZertGraf/aeol/rnnoise"
)

type captureProcessor struct {
	aec      *echoCanceller
	denoiser *rnnoise.Denoiser
}

func newCaptureProcessor() *captureProcessor {
	return &captureProcessor{aec: newEchoCanceller(), denoiser: rnnoise.New()}
}

func (p *captureProcessor) ResetNoiseSuppression() {
	p.denoiser.Reset()
}

func (p *captureProcessor) ResetEchoCancellation() {
	p.aec.Reset()
}

func (p *captureProcessor) Process(samples, reference []float32, echoCancellation, noiseSuppression bool) {
	if echoCancellation || noiseSuppression {
		for index, sample := range samples {
			if math.IsNaN(float64(sample)) || math.IsInf(float64(sample), 0) {
				sample = 0
			}
			samples[index] = max(-1, min(1, sample))
		}
	}
	if echoCancellation {
		p.aec.Process(samples, reference)
	}
	if noiseSuppression {
		for index, sample := range samples {
			if math.IsNaN(float64(sample)) || math.IsInf(float64(sample), 0) {
				sample = 0
			}
			samples[index] = max(-1, min(1, sample)) * 32768
		}
		p.denoiser.ProcessFrame(samples)
		for index, sample := range samples {
			sample /= 32768
			if math.IsNaN(float64(sample)) || math.IsInf(float64(sample), 0) {
				sample = 0
			}
			samples[index] = max(-1, min(1, sample))
		}
	}
}

func (p *captureProcessor) Close() {
	if p.denoiser != nil {
		p.denoiser.Close()
	}
}

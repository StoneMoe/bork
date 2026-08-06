package audio

import (
	"math"

	"github.com/ZertGraf/aeol/fft"
)

const (
	echoFFTSize        = 1024
	echoBins           = echoFFTSize/2 + 1
	echoPartitions     = 30 // 300 ms filter tail, with 290 ms used for delay acquisition.
	echoStepSize       = 0.1
	echoLeak           = 0.9999
	echoMinCorrelation = 0.15
	echoMaxFilterPower = 1.0
	echoTrustFrames    = 3
)

// ponytail: linear 300 ms AEC; move to a platform AEC only if hardware tests
// show nonlinear speaker distortion needs it.
type echoCanceller struct {
	fft              fft.FFT
	history          [echoPartitions][echoBins]complex64
	historyTime      [echoPartitions][FrameSamples]float32
	filter           [echoPartitions][echoBins]complex64
	historyAt        int
	overlap          [FrameSamples]float32
	time             [echoFFTSize]float32
	errorTime        [echoFFTSize]float32
	spectrum         [echoBins]complex64
	error            [echoBins]complex64
	power            [echoBins]float32
	framesSeen       int
	delaySamples     int
	delayCorrelation float64
	delayTrusted     int
}

func newEchoCanceller() *echoCanceller {
	return &echoCanceller{fft: fft.DefaultFactory(echoFFTSize)}
}

func (c *echoCanceller) Reset() {
	for partition := range echoPartitions {
		clear(c.history[partition][:])
		clear(c.historyTime[partition][:])
		clear(c.filter[partition][:])
	}
	clear(c.overlap[:])
	clear(c.time[:])
	clear(c.errorTime[:])
	clear(c.spectrum[:])
	clear(c.error[:])
	clear(c.power[:])
	c.historyAt = 0
	c.framesSeen = 0
	c.delaySamples = 0
	c.delayCorrelation = 0
	c.delayTrusted = 0
}

func (c *echoCanceller) Process(capture, reference []float32) {
	c.historyAt = (c.historyAt + echoPartitions - 1) % echoPartitions
	referenceFrame := &c.historyTime[c.historyAt]
	clear(referenceFrame[:])
	for index := 0; index < min(len(reference), FrameSamples); index++ {
		sample := reference[index]
		if math.IsNaN(float64(sample)) || math.IsInf(float64(sample), 0) {
			sample = 0
		}
		referenceFrame[index] = max(-1, min(1, sample))
	}
	c.forward(referenceFrame[:], &c.history[c.historyAt])
	captureEnergy := frameEnergy(capture)
	c.updateDelay(capture, captureEnergy)
	saturated := false
	for _, sample := range capture {
		if sample <= -0.99 || sample >= 0.99 {
			saturated = true
			break
		}
	}

	clear(c.spectrum[:])
	for partition := range echoPartitions {
		x := &c.history[(c.historyAt+partition)%echoPartitions]
		h := &c.filter[partition]
		for bin := range echoBins {
			c.spectrum[bin] += h[bin] * x[bin]
		}
	}
	c.inverse(&c.spectrum, c.time[:])
	for index := range FrameSamples {
		predicted := c.time[index] + c.overlap[index]
		if math.IsNaN(float64(predicted)) || math.IsInf(float64(predicted), 0) || math.Abs(float64(predicted)) > 4 {
			c.Reset()
			return
		}
	}
	clear(c.errorTime[:])
	residualEnergy := float64(0)
	for index := range FrameSamples {
		predicted := c.time[index] + c.overlap[index]
		residual := capture[index] - predicted
		c.errorTime[index] = residual
		residualEnergy += float64(residual * residual)
		c.overlap[index] = c.time[FrameSamples+index]
	}

	trusted := c.delayTrusted >= echoTrustFrames
	if !saturated && trusted && residualEnergy < captureEnergy*0.95 {
		copy(capture, c.errorTime[:FrameSamples])
	}
	if saturated || captureEnergy < 1e-8 || !trusted {
		return
	}
	c.forward(c.errorTime[:], &c.error)
	maxPower := float32(0)
	for bin := range echoBins {
		power := float32(0)
		for partition := range echoPartitions {
			x := c.history[(c.historyAt+partition)%echoPartitions][bin]
			power += real(x)*real(x) + imag(x)*imag(x)
		}
		c.power[bin] = power
		maxPower = max(maxPower, power)
	}
	powerFloor := max(float32(1e-4), maxPower*1e-2)
	for bin := range echoBins {
		scale := float32(echoStepSize) / max(c.power[bin], powerFloor)
		er, ei := real(c.error[bin]), imag(c.error[bin])
		filterPower := float64(0)
		for partition := range echoPartitions {
			x := c.history[(c.historyAt+partition)%echoPartitions][bin]
			xr, xi := real(x), imag(x)
			coefficient := c.filter[partition][bin]*echoLeak + complex((xr*er+xi*ei)*scale, (xr*ei-xi*er)*scale)
			if !finiteComplex(coefficient) {
				c.Reset()
				return
			}
			c.filter[partition][bin] = coefficient
			filterPower += float64(real(coefficient)*real(coefficient) + imag(coefficient)*imag(coefficient))
		}
		if math.IsNaN(filterPower) || math.IsInf(filterPower, 0) {
			c.Reset()
			return
		}
		if filterPower > echoMaxFilterPower {
			limit := float32(math.Sqrt(echoMaxFilterPower / filterPower))
			for partition := range echoPartitions {
				c.filter[partition][bin] *= complex(limit, 0)
			}
		}
	}
	for partition := range echoPartitions {
		c.constrain(&c.filter[partition])
	}
}

func (c *echoCanceller) updateDelay(capture []float32, captureEnergy float64) {
	c.framesSeen++
	if captureEnergy < 1e-8 {
		c.setDelayConfidence(c.delaySamples, 0)
		return
	}
	if c.framesSeen%10 != 1 {
		c.setDelayConfidence(c.delaySamples, c.correlationAtDelay(capture, captureEnergy, c.delaySamples))
		return
	}
	const coarseStep = 8
	var coarseCapture [FrameSamples / coarseStep]float64
	var coarseCaptureEnergy float64
	for block := range coarseCapture {
		for offset := range coarseStep {
			coarseCapture[block] += float64(capture[block*coarseStep+offset])
		}
		coarseCaptureEnergy += coarseCapture[block] * coarseCapture[block]
	}
	coarseDelay := c.delaySamples
	bestCoarseCorrelation := float64(0)
	for delay := 0; delay <= (echoPartitions-1)*FrameSamples; delay += coarseStep {
		correlation := c.coarseCorrelationAtDelay(coarseCapture[:], coarseCaptureEnergy, delay, coarseStep)
		if correlation > bestCoarseCorrelation {
			coarseDelay = delay
			bestCoarseCorrelation = correlation
		}
	}
	bestDelay := c.delaySamples
	bestCorrelation := c.correlationAtDelay(capture, captureEnergy, bestDelay)
	for delay := max(0, coarseDelay-coarseStep+1); delay <= min((echoPartitions-1)*FrameSamples, coarseDelay+coarseStep-1); delay++ {
		correlation := c.correlationAtDelay(capture, captureEnergy, delay)
		if correlation > bestCorrelation {
			bestDelay = delay
			bestCorrelation = correlation
		}
	}
	c.setDelayConfidence(bestDelay, bestCorrelation)
}

func (c *echoCanceller) setDelayConfidence(delay int, correlation float64) {
	c.delayCorrelation = correlation
	if correlation < echoMinCorrelation {
		c.delayTrusted = 0
		return
	}
	if delay != c.delaySamples {
		c.delaySamples = delay
		c.delayTrusted = 1
		return
	}
	c.delayTrusted = min(c.delayTrusted+1, echoTrustFrames)
}

func (c *echoCanceller) coarseCorrelationAtDelay(capture []float64, captureEnergy float64, delay, step int) float64 {
	if captureEnergy < 1e-8 {
		return 0
	}
	var dot, referenceEnergy float64
	for block, sample := range capture {
		reference := float64(0)
		for offset := range step {
			reference += float64(c.referenceSample(delay, block*step+offset))
		}
		dot += sample * reference
		referenceEnergy += reference * reference
	}
	if referenceEnergy < 1e-8 {
		return 0
	}
	return math.Abs(dot) / math.Sqrt(captureEnergy*referenceEnergy)
}

func (c *echoCanceller) correlationAtDelay(capture []float32, captureEnergy float64, delay int) float64 {
	var dot, referenceEnergy float64
	for index, sample := range capture {
		if index == FrameSamples {
			break
		}
		value := float64(c.referenceSample(delay, index))
		dot += float64(sample) * value
		referenceEnergy += value * value
	}
	if referenceEnergy < 1e-8 {
		return 0
	}
	return math.Abs(dot) / math.Sqrt(captureEnergy*referenceEnergy)
}

func (c *echoCanceller) referenceSample(delay, index int) float32 {
	relative := index - delay
	if relative >= 0 {
		return c.historyTime[c.historyAt][relative]
	}
	blocksBack := (-relative + FrameSamples - 1) / FrameSamples
	if blocksBack >= echoPartitions {
		return 0
	}
	return c.historyTime[(c.historyAt+blocksBack)%echoPartitions][relative+blocksBack*FrameSamples]
}

func finiteComplex(value complex64) bool {
	return !math.IsNaN(float64(real(value))) && !math.IsInf(float64(real(value)), 0) &&
		!math.IsNaN(float64(imag(value))) && !math.IsInf(float64(imag(value)), 0)
}

func (c *echoCanceller) forward(samples []float32, spectrum *[echoBins]complex64) {
	clear(c.time[:])
	copy(c.time[:FrameSamples], samples)
	c.fft.Forward(c.time[:])
	unpackSpectrum(c.time[:], spectrum)
}

func (c *echoCanceller) inverse(spectrum *[echoBins]complex64, destination []float32) {
	packSpectrum(spectrum, c.time[:])
	c.fft.Inverse(c.time[:])
	copy(destination, c.time[:len(destination)])
}

func (c *echoCanceller) constrain(spectrum *[echoBins]complex64) {
	packSpectrum(spectrum, c.time[:])
	c.fft.Inverse(c.time[:])
	clear(c.time[FrameSamples:])
	c.fft.Forward(c.time[:])
	unpackSpectrum(c.time[:], spectrum)
}

func unpackSpectrum(packed []float32, spectrum *[echoBins]complex64) {
	spectrum[0] = complex(packed[0], 0)
	spectrum[echoBins-1] = complex(packed[1], 0)
	for bin := 1; bin < echoBins-1; bin++ {
		spectrum[bin] = complex(packed[2*bin], packed[2*bin+1])
	}
}

func packSpectrum(spectrum *[echoBins]complex64, packed []float32) {
	packed[0] = real(spectrum[0])
	packed[1] = real(spectrum[echoBins-1])
	for bin := 1; bin < echoBins-1; bin++ {
		packed[2*bin] = real(spectrum[bin])
		packed[2*bin+1] = imag(spectrum[bin])
	}
}

func frameEnergy(samples []float32) float64 {
	var energy float64
	for _, sample := range samples {
		energy += float64(sample * sample)
	}
	return energy
}

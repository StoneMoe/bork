package audio

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

func TestCaptureProcessorBypassesDisabledStages(t *testing.T) {
	processor := newCaptureProcessor()
	defer processor.Close()

	samples := make([]float32, FrameSamples)
	reference := make([]float32, FrameSamples)
	for index := range samples {
		samples[index] = float32(index-FrameSamples/2) / FrameSamples
		reference[index] = -samples[index]
	}
	wantSamples := slices.Clone(samples)
	wantReference := slices.Clone(reference)
	processor.Process(samples, reference, false, false)
	if !slices.Equal(samples, wantSamples) || !slices.Equal(reference, wantReference) {
		t.Fatal("disabled capture processing changed audio")
	}
}

func TestCaptureProcessorSuppressesNoise(t *testing.T) {
	processor := newCaptureProcessor()
	defer processor.Close()

	random := rand.New(rand.NewPCG(1, 2))
	reference := make([]float32, FrameSamples)
	var inputEnergy, outputEnergy float64
	for frame := 0; frame < 100; frame++ {
		samples := make([]float32, FrameSamples)
		for index := range samples {
			samples[index] = (random.Float32()*2 - 1) * 0.08
		}
		if frame >= 50 {
			inputEnergy += energy(samples)
		}
		processor.Process(samples, reference, false, true)
		if frame >= 50 {
			outputEnergy += energy(samples)
		}
	}
	if outputEnergy >= inputEnergy*0.8 {
		t.Fatalf("RNNoise did not suppress noise: input=%f output=%f", inputEnergy, outputEnergy)
	}
	if outputEnergy <= inputEnergy*0.1 {
		t.Fatalf("RNNoise muted the signal instead of suppressing it: input=%f output=%f", inputEnergy, outputEnergy)
	}
}

func TestCaptureProcessorCancelsDelayedReverberantEcho(t *testing.T) {
	processor := newCaptureProcessor()
	defer processor.Close()

	const (
		frames          = 700
		warmup          = 500
		delay           = 5*FrameSamples + 137
		reflectionDelay = delay + 673
	)
	random := rand.New(rand.NewPCG(3, 4))
	referenceStream := make([]float32, frames*FrameSamples)
	for index := range referenceStream {
		referenceStream[index] = (random.Float32()*2 - 1) * 0.2
	}
	var inputEnergy, outputEnergy float64
	for frame := range frames {
		reference := referenceStream[frame*FrameSamples : (frame+1)*FrameSamples]
		samples := make([]float32, FrameSamples)
		for index := range samples {
			position := frame*FrameSamples + index
			if position >= delay {
				samples[index] += referenceStream[position-delay] * 0.5
			}
			if position >= reflectionDelay {
				samples[index] += referenceStream[position-reflectionDelay] * 0.18
			}
		}
		if frame >= warmup {
			inputEnergy += energy(samples)
		}
		processor.Process(samples, reference, true, false)
		if frame >= warmup {
			outputEnergy += energy(samples)
		}
	}
	if outputEnergy >= inputEnergy*0.4 {
		t.Fatalf("AEC did not cancel delayed echo: input=%f output=%f delay=%d correlation=%f", inputEnergy, outputEnergy, processor.aec.delaySamples, processor.aec.delayCorrelation)
	}
}

func TestCaptureProcessorPreservesNearSpeechDuringColdStartAndTrainedDoubleTalk(t *testing.T) {
	processor := newCaptureProcessor()
	defer processor.Close()

	const (
		frames           = 900
		coldTalkEnd      = 300
		trainedTalkStart = 600
		delay            = 4*FrameSamples + 91
	)
	random := rand.New(rand.NewPCG(5, 6))
	referenceStream := make([]float32, frames*FrameSamples)
	for index := range referenceStream {
		referenceStream[index] = (random.Float32()*2 - 1) * 0.18
	}
	var coldNearEnergy, coldNearProjection float64
	var nearEnergy, nearProjection, residualEnergy, echoEnergy float64
	for frame := range frames {
		reference := referenceStream[frame*FrameSamples : (frame+1)*FrameSamples]
		samples := make([]float32, FrameSamples)
		near := make([]float32, FrameSamples)
		for index := range samples {
			position := frame*FrameSamples + index
			if position >= delay {
				samples[index] = referenceStream[position-delay] * 0.45
			}
			if frame >= trainedTalkStart {
				echoEnergy += float64(samples[index] * samples[index])
			}
			if frame < coldTalkEnd || frame >= trainedTalkStart {
				near[index] = 0.16*float32(math.Sin(2*math.Pi*180*float64(position)/SampleRate)) +
					0.06*float32(math.Sin(2*math.Pi*360*float64(position)/SampleRate))
				samples[index] += near[index]
			}
		}
		processor.Process(samples, reference, true, false)
		if frame < coldTalkEnd {
			for index, output := range samples {
				coldNearEnergy += float64(near[index] * near[index])
				coldNearProjection += float64(output * near[index])
			}
		} else if frame >= trainedTalkStart {
			for index, output := range samples {
				nearEnergy += float64(near[index] * near[index])
				nearProjection += float64(output * near[index])
				difference := output - near[index]
				residualEnergy += float64(difference * difference)
			}
		}
	}
	coldGain := coldNearProjection / coldNearEnergy
	if coldGain < 0.8 || coldGain > 1.2 {
		t.Fatalf("AEC changed near speech gain during cold-start double-talk: %f", coldGain)
	}
	gain := nearProjection / nearEnergy
	if gain < 0.8 || gain > 1.2 {
		t.Fatalf("AEC changed near speech gain during double-talk: %f", gain)
	}
	if residualEnergy >= echoEnergy*0.5 {
		t.Fatalf("AEC lost cancellation during double-talk: echo=%f residual=%f delay=%d correlation=%f trusted=%d", echoEnergy, residualEnergy, processor.aec.delaySamples, processor.aec.delayCorrelation, processor.aec.delayTrusted)
	}
}

func TestCaptureProcessorDoesNotInjectLearnedEchoAfterPathDisappears(t *testing.T) {
	processor := newCaptureProcessor()
	defer processor.Close()

	const (
		trainingFrames = 500
		testFrames     = 50
		delay          = 3*FrameSamples + 73
	)
	random := rand.New(rand.NewPCG(9, 10))
	referenceStream := make([]float32, (trainingFrames+testFrames)*FrameSamples)
	for index := range referenceStream {
		referenceStream[index] = (random.Float32()*2 - 1) * 0.2
	}
	for frame := range trainingFrames {
		reference := referenceStream[frame*FrameSamples : (frame+1)*FrameSamples]
		samples := make([]float32, FrameSamples)
		for index := range samples {
			position := frame*FrameSamples + index
			if position >= delay {
				samples[index] = referenceStream[position-delay] * 0.5
			}
		}
		processor.Process(samples, reference, true, false)
	}
	for frame := trainingFrames; frame < trainingFrames+testFrames; frame++ {
		reference := referenceStream[frame*FrameSamples : (frame+1)*FrameSamples]
		samples := make([]float32, FrameSamples)
		for index := range samples {
			position := frame*FrameSamples + index
			samples[index] = 0.12 * float32(math.Sin(2*math.Pi*197*float64(position)/SampleRate))
		}
		want := slices.Clone(samples)
		processor.Process(samples, reference, true, false)
		if !slices.Equal(samples, want) {
			t.Fatal("AEC injected its stale echo estimate after the acoustic path disappeared")
		}
	}
}

func TestCaptureProcessorContainsNonFiniteInput(t *testing.T) {
	processor := newCaptureProcessor()
	defer processor.Close()
	samples := make([]float32, FrameSamples)
	reference := make([]float32, FrameSamples)
	samples[0] = float32(math.NaN())
	samples[1] = float32(math.Inf(1))
	reference[2] = float32(math.Inf(-1))
	processor.Process(samples, reference, true, true)
	for index, sample := range samples {
		if math.IsNaN(float64(sample)) || math.IsInf(float64(sample), 0) || sample < -1 || sample > 1 {
			t.Fatalf("processed sample %d is invalid: %f", index, sample)
		}
	}
}

func BenchmarkCaptureProcessor(b *testing.B) {
	processor := newCaptureProcessor()
	defer processor.Close()
	random := rand.New(rand.NewPCG(7, 8))
	reference := make([]float32, FrameSamples)
	input := make([]float32, FrameSamples)
	samples := make([]float32, FrameSamples)
	for index := range input {
		reference[index] = (random.Float32()*2 - 1) * 0.2
		input[index] = reference[index]*0.4 + (random.Float32()*2-1)*0.02
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		copy(samples, input)
		processor.Process(samples, reference, true, true)
	}
}

func energy(samples []float32) float64 {
	var total float64
	for _, sample := range samples {
		if math.IsNaN(float64(sample)) || math.IsInf(float64(sample), 0) {
			return math.Inf(1)
		}
		total += float64(sample * sample)
	}
	return total
}

package audio

import (
	"math"
	"testing"
)

func TestPlaybackGainFactor(t *testing.T) {
	for _, test := range []struct {
		percent int64
		want    float32
	}{
		{0, 0}, {50, 0.25}, {100, 1}, {150, 2.25}, {200, 4},
	} {
		if got := playbackGainFactor(test.percent); got != test.want {
			t.Fatalf("playbackGainFactor(%d) = %v, want %v", test.percent, got, test.want)
		}
	}
}

func TestCaptureMeter(t *testing.T) {
	level, clipped := captureMeter([]float32{0.5, -0.5})
	if math.Abs(level-0.5) > 1e-6 || clipped {
		t.Fatalf("captureMeter = (%v, %v), want (0.5, false)", level, clipped)
	}
	if _, clipped = captureMeter([]float32{0.999}); !clipped {
		t.Fatal("captureMeter did not report clipping at the threshold")
	}
	level, clipped = captureMeter([]float32{float32(math.NaN())})
	if level != 0 || clipped {
		t.Fatalf("captureMeter with NaN = (%v, %v), want (0, false)", level, clipped)
	}

	var window captureMeterWindow
	for range captureMeterFrames - 1 {
		if _, ready := window.add(0.5); ready {
			t.Fatal("capture meter window completed early")
		}
	}
	level, ready := window.add(1)
	want := math.Sqrt((float64(captureMeterFrames-1)*0.25 + 1) / captureMeterFrames)
	if !ready || math.Abs(level-want) > 1e-6 {
		t.Fatalf("capture meter window = (%v, %v), want (%v, true)", level, ready, want)
	}
}

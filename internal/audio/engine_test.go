package audio

import "testing"

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

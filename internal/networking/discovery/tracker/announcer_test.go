package tracker

import (
	"testing"
	"time"
)

func TestEffectiveAnnounceInterval(t *testing.T) {
	tests := []struct {
		name      string
		provider  time.Duration
		announces int
		expected  time.Duration
	}{
		{name: "initial", provider: 5 * time.Minute, announces: 2, expected: 5 * time.Second},
		{name: "minimum", provider: time.Second, announces: 3, expected: 5 * time.Second},
		{name: "provider", provider: 20 * time.Second, announces: 3, expected: 20 * time.Second},
		{name: "maximum", provider: 5 * time.Minute, announces: 3, expected: 30 * time.Second},
	}

	for _, test := range tests {
		if actual := effectiveAnnounceInterval(test.provider, test.announces); actual != test.expected {
			t.Errorf("%s: got %s, want %s", test.name, actual, test.expected)
		}
	}
}

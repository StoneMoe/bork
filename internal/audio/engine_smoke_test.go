package audio

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"bork/internal/media"
)

func TestAudioHardwareSmoke(t *testing.T) {
	if os.Getenv("BORK_AUDIO_SMOKE") != "1" {
		t.Skip("set BORK_AUDIO_SMOKE=1 to exercise the default audio devices")
	}
	engine, err := New(Options{MaxEncodedFrameBytes: 1200}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer engine.Close()
	if !engine.Status().Available {
		t.Fatal("default capture and playback devices are unavailable")
	}
	flow := media.NewFlow()
	if _, err := engine.Start(flow); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	select {
	case <-ctx.Done():
		t.Fatal("capture device produced no Opus frames")
	case <-flow.SendReady():
		if !flow.ConsumeSend(func(media.SendFrame) {}) {
			t.Fatal("capture notification contained no Opus frame")
		}
	}
	if state := engine.Stop(); state.Running || state.Error != "" {
		t.Fatalf("audio state after Stop() = %#v", state)
	}
}

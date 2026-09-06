package app

import (
	"encoding/json"
	"slices"
	"testing"

	"bork/internal/config"

	wailslogger "github.com/wailsapp/wails/v2/pkg/logger"
)

type recordingWailsLogger struct {
	wailslogger.Logger
	messages []string
}

func (logger *recordingWailsLogger) Trace(message string) {
	logger.messages = append(logger.messages, message)
}

func (logger *recordingWailsLogger) Info(message string) {
	logger.messages = append(logger.messages, message)
}

func TestPrivateWailsLogger_drops_snapshot_payload_traces(t *testing.T) {
	application := NewApp(config.AppConfig{}, nil)
	application.nickname = "private-nickname"
	contents, err := json.Marshal(application.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	sink := &recordingWailsLogger{}
	logger := privateWailsLogger{sink}
	logger.Trace("json call result data: " + string(contents) + "\n")
	logger.Trace("ordinary trace")
	logger.Info("ordinary info")
	if !slices.Equal(sink.messages, []string{"ordinary trace", "ordinary info"}) {
		t.Fatal("RPC payload reached the log sink or ordinary logging was suppressed")
	}
}

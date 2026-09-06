//go:build game_proxy

package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"bork/internal/gameproxy"
	"bork/internal/gameproxy/iwan"
)

func TestProjectGameProxyStatus_qualityContractAndIsolation(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	status := gameproxy.Status{Quality: iwan.LinkQuality{
		ObservedAt: now, RTTMillis: new(12.5), LossPercent: new(50.0),
		History: []iwan.LinkSample{
			{At: now.Add(-time.Second), Generation: 1, RTTMillis: new(12.5)},
			{At: now, Generation: 2, LossPercent: 100},
		},
	}, TrafficHistory: []gameproxy.TrafficSample{
		{At: now.Add(123456789 * time.Nanosecond), Generation: 1, UploadRate: 30, DownloadRate: 60},
		{At: now.Add(time.Second), Generation: 2},
	}}
	snapshot := projectGameProxyStatus(status)
	encoded, err := json.Marshal(snapshot.Quality)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"observedAt":"2026-09-05T12:00:00Z","rttMs":12.5,"lossPercent":50,"history":[{"at":"2026-09-05T11:59:59Z","generation":1,"rttMs":12.5,"lossPercent":0},{"at":"2026-09-05T12:00:00Z","generation":2,"rttMs":null,"lossPercent":100}]}`
	if string(encoded) != want {
		t.Fatalf("quality JSON = %s", encoded)
	}
	encoded, err = json.Marshal(snapshot.TrafficHistory)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `[{"at":"2026-09-05T12:00:00.123456789Z","generation":1,"uploadRate":30,"downloadRate":60},{"at":"2026-09-05T12:00:01Z","generation":2,"uploadRate":0,"downloadRate":0}]` {
		t.Fatalf("traffic history JSON = %s", encoded)
	}
	*status.Quality.RTTMillis, *status.Quality.LossPercent, *status.Quality.History[0].RTTMillis = 99, 99, 99
	status.Quality.History[1].Generation = 99
	status.TrafficHistory[0].UploadRate = 99
	if *snapshot.Quality.RTTMillis != 12.5 || *snapshot.Quality.LossPercent != 50 ||
		*snapshot.Quality.History[0].RTTMillis != 12.5 || snapshot.Quality.History[1].Generation != 2 || snapshot.TrafficHistory[0].UploadRate != 30 {
		t.Fatal("app projection retained shared telemetry")
	}
	*snapshot.Quality.History[0].RTTMillis = 88
	snapshot.TrafficHistory[0].UploadRate = 88
	if *status.Quality.History[0].RTTMillis != 99 || status.TrafficHistory[0].UploadRate != 99 {
		t.Fatal("app consumer mutated input history")
	}
	empty, err := json.Marshal(projectGameProxyStatus(gameproxy.Status{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(empty), `"quality":{"observedAt":"0001-01-01T00:00:00Z","rttMs":null,"lossPercent":null,"history":[]}`) ||
		!strings.Contains(string(empty), `"trafficHistory":[]`) {
		t.Fatalf("unknown quality JSON = %s", empty)
	}
}

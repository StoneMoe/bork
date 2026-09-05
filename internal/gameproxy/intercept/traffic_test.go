package intercept

import (
	"testing"
	"time"
)

func TestRelay_Traffic_calculates_rates_since_previous_sample(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	relay := newTestRelay(t, fakeRules{selected: true}, &fakeDialer{})
	relay.options.Clock = clock
	if initial := relay.Traffic(); initial != (TrafficStats{}) {
		t.Fatalf("initial traffic = %#v", initial)
	}
	relay.addUpload(600)
	relay.addDownload(1000)
	clock.mu.Lock()
	clock.now = clock.now.Add(2 * time.Second)
	clock.mu.Unlock()

	traffic := relay.Traffic()
	if traffic != (TrafficStats{UploadBytes: 600, DownloadBytes: 1000, UploadRate: 300, DownloadRate: 500}) {
		t.Fatalf("traffic = %#v", traffic)
	}
}

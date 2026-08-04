package tracker

import (
	"context"
	"crypto/sha256"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"
)

func TestLiveDefaultHTTPProviders(t *testing.T) {
	if os.Getenv("BORK_LIVE_TRACKER_TEST") != "1" {
		t.Skip("set BORK_LIVE_TRACKER_TEST=1 to contact public trackers")
	}
	providers := []string{
		"https://tracker.zhuqiy.com/announce",
		"http://tracker.renfei.net:8080/announce",
		"http://tracker.mywaifu.best:6969/announce",
	}
	digest := sha256.Sum256([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	var infoHash [20]byte
	copy(infoHash[:], digest[:])
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, raw := range providers {
		t.Run(raw, func(t *testing.T) {
			configured, err := parseProvider(raw)
			if err != nil {
				t.Fatal(err)
			}
			announcer, err := newAnnouncer([]string{raw}, infoHash, testTrackerIdentity, AnnounceCandidate{Port: 49152}, nil, logger)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			timing := normalizeTiming(announcer.timing)
			firstRegistration := announcer.registration(configured, AnnounceCandidate{Port: 49152})
			response, err := announcer.httpAnnounce(ctx, configured, firstRegistration, true, timing)
			if err != nil {
				t.Fatal(err)
			}
			second, err := newAnnouncer([]string{raw}, infoHash, [32]byte{2}, AnnounceCandidate{Port: 49153}, nil, logger)
			if err != nil {
				t.Fatal(err)
			}
			secondRegistration := second.registration(configured, AnnounceCandidate{Port: 49153})
			if _, err := second.httpAnnounce(ctx, configured, secondRegistration, true, timing); err != nil {
				t.Fatal(err)
			}
			response, err = announcer.httpAnnounce(ctx, configured, firstRegistration, false, timing)
			if err != nil {
				t.Fatal(err)
			}
			observed := observedRegistrationAddress(response, firstRegistration.candidate)
			t.Logf("interval=%s observed=%s peers=%v", response.interval, observed, response.peers)
		})
	}
}

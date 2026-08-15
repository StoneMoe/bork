package discovery

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/netip"
	"time"
)

type Source string

const (
	SourceLocal            Source = "local"
	SourceMDNS             Source = "mdns"
	SourceTracker          Source = "tracker"
	SourceTopology         Source = "topology"
	SourceHistoricalRemote Source = "historical-remote"
)

type Hint struct {
	Address   netip.AddrPort
	Source    Source
	ExpiresAt time.Time
}

func newPeerHint() (string, error) {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate discovery peer hint: %w", err)
	}
	return hex.EncodeToString(random[:]), nil
}

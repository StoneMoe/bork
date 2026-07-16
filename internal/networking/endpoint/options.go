package endpoint

import "time"

const (
	defaultListenAddress = "[::]:0"
	defaultSTUNTimeout   = 2500 * time.Millisecond
	defaultSTUNRefresh   = 5 * time.Minute
)

var defaultSTUNServers = []string{
	"stun.cloudflare.com:3478",
	"stun.l.google.com:19302",
}

type Options struct {
	ListenAddress string
	STUNServers   []string
	STUNTimeout   time.Duration
	STUNRefresh   time.Duration
}

type STUNResult struct {
	Server        string `json:"server"`
	MappedAddress string `json:"mappedAddress,omitempty"`
	RTTMillis     int64  `json:"rttMillis,omitempty"`
	Error         string `json:"error,omitempty"`
}

type Snapshot struct {
	ListenAddress string       `json:"listenAddress"`
	Candidates    []Candidate  `json:"candidates"`
	STUN          []STUNResult `json:"stun"`
}

func DefaultOptions() Options {
	return Options{
		ListenAddress: defaultListenAddress,
		STUNServers:   append([]string(nil), defaultSTUNServers...),
		STUNTimeout:   defaultSTUNTimeout,
		STUNRefresh:   defaultSTUNRefresh,
	}
}

func normalizeOptions(options Options) Options {
	if options.ListenAddress == "" {
		options.ListenAddress = defaultListenAddress
	}
	if options.STUNServers == nil {
		options.STUNServers = append([]string(nil), defaultSTUNServers...)
	} else {
		options.STUNServers = append([]string{}, options.STUNServers...)
	}
	if options.STUNTimeout <= 0 {
		options.STUNTimeout = defaultSTUNTimeout
	}
	if options.STUNRefresh < 0 {
		options.STUNRefresh = 0
	}
	return options
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	snapshot.Candidates = append([]Candidate{}, snapshot.Candidates...)
	snapshot.STUN = append([]STUNResult{}, snapshot.STUN...)
	return snapshot
}

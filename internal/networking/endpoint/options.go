package endpoint

import "time"

const (
	defaultListenAddress = "[::]:0"
	defaultSTUNTimeout   = 2500 * time.Millisecond
	defaultSTUNRefresh   = 5 * time.Minute
)

type Options struct {
	ListenAddress string
	STUNServers   []string
	STUNTimeout   time.Duration
	STUNRefresh   time.Duration
}

type STUNResult struct {
	Server        string `json:"server"`
	Family        string `json:"family,omitempty"`
	MappedAddress string `json:"mappedAddress,omitempty"`
	RTTMillis     int64  `json:"rttMillis,omitempty"`
	Error         string `json:"error,omitempty"`
}

type Snapshot struct {
	ListenAddress string       `json:"listenAddress"`
	Candidates    []Candidate  `json:"candidates"`
	STUN          []STUNResult `json:"stun"`
}

func (s Snapshot) Clone() Snapshot {
	s.Candidates = append([]Candidate{}, s.Candidates...)
	s.STUN = append([]STUNResult{}, s.STUN...)
	return s
}

func DefaultOptions() Options {
	return Options{
		ListenAddress: defaultListenAddress,
		STUNServers:   []string{},
		STUNTimeout:   defaultSTUNTimeout,
		STUNRefresh:   defaultSTUNRefresh,
	}
}

func normalizeOptions(options Options) Options {
	if options.ListenAddress == "" {
		options.ListenAddress = defaultListenAddress
	}
	options.STUNServers = append([]string{}, options.STUNServers...)
	if options.STUNTimeout <= 0 {
		options.STUNTimeout = defaultSTUNTimeout
	}
	if options.STUNRefresh < 0 {
		options.STUNRefresh = 0
	}
	return options
}

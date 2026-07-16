package endpoint

import (
	"net/netip"
	"testing"
)

func TestIsUsableHostAddress(t *testing.T) {
	tests := []struct {
		address string
		want    bool
	}{
		{address: "192.168.1.10", want: true},
		{address: "203.0.113.4", want: true},
		{address: "fd00::10", want: true},
		{address: "2001:db8::10", want: true},
		{address: "127.0.0.1", want: false},
		{address: "::1", want: false},
		{address: "0.0.0.0", want: false},
		{address: "fe80::1", want: false},
		{address: "224.0.0.1", want: false},
	}
	for _, test := range tests {
		t.Run(test.address, func(t *testing.T) {
			address := netip.MustParseAddr(test.address)
			if got := isUsableHostAddress(address); got != test.want {
				t.Fatalf("isUsableHostAddress(%s) = %v, want %v", address, got, test.want)
			}
		})
	}
}

func TestHostCandidatesRespectsExplicitBind(t *testing.T) {
	candidates, err := hostCandidates(netip.MustParseAddr("192.0.2.10"), 7778)
	if err != nil {
		t.Fatalf("hostCandidates() error = %v", err)
	}
	if len(candidates) != 1 || candidates[0].Address != "192.0.2.10:7778" {
		t.Fatalf("hostCandidates() = %#v", candidates)
	}
	if candidates, err := hostCandidates(netip.MustParseAddr("127.0.0.1"), 7778); err != nil || len(candidates) != 0 {
		t.Fatalf("loopback hostCandidates() = %#v, %v", candidates, err)
	}
}

func TestIPv4WildcardCandidatesExcludeIPv6(t *testing.T) {
	candidates, err := hostCandidates(netip.IPv4Unspecified(), 7778)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		if candidate.Family != "ipv4" {
			t.Fatalf("IPv4 wildcard produced candidate %#v", candidate)
		}
	}
}

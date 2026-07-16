package config

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseConfigDefault(t *testing.T) {
	cfg, err := ParseConfig(nil, io.Discard)
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	if cfg.Mode != ModeGUI {
		t.Fatalf("Mode = %q, want %q", cfg.Mode, ModeGUI)
	}
	if cfg.UDPListen != "[::]:0" || len(cfg.STUNServers) != 2 {
		t.Fatalf("network config = %#v", cfg)
	}
}

func TestParseConfigRelayPeer(t *testing.T) {
	cfg, err := ParseConfig([]string{"--relay-peer", "--join", "invite"}, io.Discard)
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	if cfg.Mode != ModeRelayPeer {
		t.Fatalf("config = %#v", cfg)
	}
}

func TestParseConfigRelayPeerRequiresInvite(t *testing.T) {
	_, err := ParseConfig([]string{"--relay-peer"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "requires") {
		t.Fatalf("ParseConfig() error = %v, want invite requirement", err)
	}
}

func TestParseConfigVersionBypassesModeValidation(t *testing.T) {
	cfg, err := ParseConfig([]string{"--relay-peer", "--version"}, io.Discard)
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	if !cfg.ShowVersion {
		t.Fatal("ShowVersion = false, want true")
	}
}

func TestParseConfigRejectsTwoInviteSources(t *testing.T) {
	_, err := ParseConfig([]string{"--join", "invite", "--join-file", "invite.txt"}, io.Discard)
	if err == nil {
		t.Fatal("ParseConfig() error = nil, want conflict error")
	}
}

func TestParseConfigNetworkOverrides(t *testing.T) {
	cfg, err := ParseConfig([]string{
		"--udp-listen", "127.0.0.1:7778",
		"--stun-servers", "stun.example:3478,stun.example:3478,192.0.2.1:19302",
	}, io.Discard)
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	if cfg.UDPListen != "127.0.0.1:7778" || len(cfg.STUNServers) != 2 {
		t.Fatalf("network config = %#v", cfg)
	}
}

func TestParseConfigCanDisableSTUN(t *testing.T) {
	cfg, err := ParseConfig([]string{"--stun-servers="}, io.Discard)
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	if cfg.STUNServers == nil || len(cfg.STUNServers) != 0 {
		t.Fatalf("STUNServers = %#v, want empty non-nil slice", cfg.STUNServers)
	}
	if options := cfg.NetworkOptions(); options.STUNServers == nil || len(options.STUNServers) != 0 {
		t.Fatalf("NetworkOptions().STUNServers = %#v, want empty non-nil slice", options.STUNServers)
	}
}

func TestParseConfigRejectsInvalidNetworkOptions(t *testing.T) {
	tests := [][]string{
		{"--udp-listen", "not-an-address"},
		{"--udp-listen", "example.com:7778"},
		{"--stun-servers", "missing-port"},
		{"--stun-servers", "example.com:0"},
	}
	for _, args := range tests {
		if _, err := ParseConfig(args, io.Discard); err == nil {
			t.Fatalf("ParseConfig(%q) error = nil", args)
		}
	}
}

func TestLoadInviteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invite.txt")
	if err := os.WriteFile(path, []byte("  bork://join/example\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	cfg, err := ParseConfig([]string{"--join-file", path}, io.Discard)
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	invite, err := cfg.LoadInvite()
	if err != nil {
		t.Fatalf("LoadInvite() error = %v", err)
	}
	if invite != "bork://join/example" {
		t.Fatalf("LoadInvite() = %q", invite)
	}
}

func TestLoadInviteFileRejectsLargeInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invite.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat(" ", 1025)), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	cfg := Config{InviteFile: path}
	if _, err := cfg.LoadInvite(); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("LoadInvite() error = %v, want size error", err)
	}
}

func TestLoadInviteFileRejectsDirectory(t *testing.T) {
	cfg := Config{InviteFile: t.TempDir()}
	if _, err := cfg.LoadInvite(); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("LoadInvite() error = %v, want regular file error", err)
	}
}

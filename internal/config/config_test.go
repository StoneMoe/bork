package config

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var testConfigRoot string

func TestMain(testingMain *testing.M) {
	root, err := os.MkdirTemp("", "bork-config-test-*")
	if err != nil {
		panic(err)
	}
	testConfigRoot = root
	oldUserConfigDir := userConfigDir
	userConfigDir = func() (string, error) { return root, nil }
	code := testingMain.Run()
	userConfigDir = oldUserConfigDir
	_ = os.RemoveAll(root)
	os.Exit(code)
}

func TestParseConfigDefault(t *testing.T) {
	cfg, err := ParseConfig(nil, io.Discard)
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	if cfg.UDPListen != "[::]:0" || !cfg.PortMapping || !slices.Equal(cfg.STUNServers, defaultSTUNServers) || !slices.Equal(cfg.TrackerURLs, defaultTrackerURLs) {
		t.Fatalf("network config = %#v", cfg)
	}
	if !cfg.NetworkOptions().EnablePortMapping {
		t.Fatal("default network options disabled port mapping")
	}
	if cfg.ConfigFile != filepath.Join(testConfigRoot, "bork", behaviorConfigFilename) {
		t.Fatalf("ConfigFile = %q", cfg.ConfigFile)
	}
}

func TestParseConfigVersion(t *testing.T) {
	cfg, err := ParseConfig([]string{"--version"}, io.Discard)
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

func TestParseConfigLoadsYAMLNetworkBehavior(t *testing.T) {
	root := t.TempDir()
	withUserConfigDir(t, root)
	path := filepath.Join(root, "bork", behaviorConfigFilename)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	contents := []byte("network:\n  udp_listen: 127.0.0.1:7778\n  stun_servers:\n    - stun.example:3478\n    - stun.example:3478\n    - 192.0.2.1:19302\n  tracker_urls: []\n  port_mapping: false\n")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(nil, io.Discard)
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	if cfg.UDPListen != "127.0.0.1:7778" || len(cfg.STUNServers) != 2 || cfg.TrackerURLs == nil || len(cfg.TrackerURLs) != 0 || cfg.PortMapping {
		t.Fatalf("network config = %#v", cfg)
	}
	if err := cfg.EnsureConfigFile(); err != nil {
		t.Fatal(err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || string(unchanged) != string(contents) {
		t.Fatalf("existing config was rewritten: %v\n%s", err, unchanged)
	}
}

func TestParseConfigRetainsCommandFlags(t *testing.T) {
	dataDir := t.TempDir()
	cfg, err := ParseConfig([]string{"--join", "invite", "--data-dir", dataDir}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Invite != "invite" || cfg.DataDir != dataDir {
		t.Fatalf("command config = %#v", cfg)
	}
}

func TestParseConfigRejectsInvalidYAMLBehavior(t *testing.T) {
	tests := []string{
		"network:\n  udp_listen: not-an-address\n",
		"network:\n  stun_servers: [missing-port]\n",
		"network:\n  tracker_urls: [ftp://tracker.example/announce]\n",
		"network:\n  unknown: true\n",
		"network: {}\n---\nnetwork: {}\n",
	}
	for index, contents := range tests {
		root := t.TempDir()
		withUserConfigDir(t, root)
		path := filepath.Join(root, "bork", behaviorConfigFilename)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ParseConfig(nil, io.Discard); err == nil {
			t.Fatalf("invalid config %d was accepted", index)
		}
	}
}

func TestEnsureConfigFileWritesDefaults(t *testing.T) {
	root := t.TempDir()
	withUserConfigDir(t, root)
	cfg, err := ParseConfig(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.EnsureConfigFile(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(cfg.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"udp_listen:", "stun_servers:", "tracker_urls:", "port_mapping: true"} {
		if !strings.Contains(string(contents), field) {
			t.Fatalf("default config missing %q:\n%s", field, contents)
		}
	}
	reloaded, err := ParseConfig(nil, io.Discard)
	if err != nil || reloaded.UDPListen != cfg.UDPListen || !slices.Equal(reloaded.STUNServers, cfg.STUNServers) {
		t.Fatalf("reloaded config = %#v, %v", reloaded, err)
	}
}

func TestPublishConfigNoReplacePreservesConcurrentFile(t *testing.T) {
	root := t.TempDir()
	temporary := filepath.Join(root, "temporary.yml")
	target := filepath.Join(root, behaviorConfigFilename)
	generated := []byte("network:\n  port_mapping: true\n")
	user := []byte("network:\n  port_mapping: false\n")
	if err := os.WriteFile(temporary, generated, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, user, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := publishConfigNoReplace(temporary, target); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(target)
	if err != nil || string(contents) != string(user) {
		t.Fatalf("concurrent config was overwritten: %v\n%s", err, contents)
	}
}

func TestPublishConfigNoReplaceReturnsLinkErrorWithoutTarget(t *testing.T) {
	root := t.TempDir()
	temporary := filepath.Join(root, "temporary.yml")
	target := filepath.Join(root, behaviorConfigFilename)
	if err := os.WriteFile(temporary, []byte("network: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkErr := errors.New("hard links unsupported")
	previous := linkConfigFile
	linkConfigFile = func(string, string) error { return linkErr }
	t.Cleanup(func() { linkConfigFile = previous })

	if err := publishConfigNoReplace(temporary, target); !errors.Is(err, linkErr) {
		t.Fatalf("publishConfigNoReplace() error = %v, want %v", err, linkErr)
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target exists after failed link: %v", err)
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

func withUserConfigDir(t *testing.T, directory string) {
	t.Helper()
	previous := userConfigDir
	userConfigDir = func() (string, error) { return directory, nil }
	t.Cleanup(func() { userConfigDir = previous })
}

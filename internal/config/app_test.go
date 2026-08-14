package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadAppConfigUsesLocalAppDataOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-specific config path")
	}
	localAppData := filepath.Join(t.TempDir(), "Local")
	t.Setenv("LocalAppData", localAppData)
	t.Setenv("AppData", filepath.Join(t.TempDir(), "Roaming"))
	config, err := LoadAppConfig()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(localAppData, "bork", "config.yml")
	if config.FilePath != want {
		t.Fatalf("config path = %q, want %q", config.FilePath, want)
	}
}

func TestLoadAppConfigFileKeepsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte("network:\n  tracker_urls: []\n  port_mapping: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := loadAppConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	network := config.Network
	if network.UDPListen != "[::]:0" || len(network.STUNServers) != 2 || network.TrackerURLs == nil || len(network.TrackerURLs) != 0 || network.PortMapping {
		t.Fatalf("config = %#v", config)
	}
}

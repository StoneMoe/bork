package config

import (
	"os"
	"path/filepath"
	"testing"
)

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

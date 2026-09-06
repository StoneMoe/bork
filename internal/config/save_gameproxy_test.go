//go:build game_proxy

package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSaveGameProxyPreservesLatestNetworkSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	initial, err := loadAppConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	initial.FilePath = path
	if err := initial.Save(); err != nil {
		t.Fatal(err)
	}
	external := initial
	external.Network = NetworkConfig{UDPListen: "127.0.0.1:4321", STUNServers: []string{}, TrackerURLs: []string{}}
	if err := external.Save(); err != nil {
		t.Fatal(err)
	}
	proxy := GameProxyConfig{Directories: []string{filepath.Clean("/new/games")}, Node: validGameProxyNode()}
	if err := initial.SaveGameProxy(proxy); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadAppConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Network, external.Network) || !reflect.DeepEqual(loaded.GameProxy, proxy) {
		t.Fatal("proxy save did not preserve the latest network settings alongside the new proxy config")
	}
}

func TestSaveGameProxyDoesNotOverwriteInvalidExternalConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	config, err := loadAppConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	config.FilePath = path
	external := []byte("network:\n  udp_listen: invalid\n")
	if err := os.WriteFile(path, external, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveGameProxy(GameProxyConfig{Node: validGameProxyNode()}); err == nil {
		t.Fatal("proxy save accepted an invalid externally edited config")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != string(external) {
		t.Fatal("proxy save overwrote the externally edited config")
	}
}

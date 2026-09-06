//go:build !game_proxy

package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDefaultAppConfigHasOnlyNetwork(t *testing.T) {
	configType := reflect.TypeFor[AppConfig]()
	if configType.NumField() != 2 || configType.Field(0).Name != "FilePath" || configType.Field(1).Name != "Network" {
		t.Fatal("default app config contains feature state")
	}
	if _, exists := configType.MethodByName("SaveGameProxy"); exists {
		t.Fatal("default app config exposes a feature method")
	}
	for name, save := range map[string]func(AppConfig) error{
		"Save":       AppConfig.Save,
		"EnsureFile": AppConfig.EnsureFile,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			config, err := loadAppConfigFile(path)
			if err != nil {
				t.Fatal(err)
			}
			config.FilePath = path
			if err := save(config); err != nil {
				t.Fatal(err)
			}
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]any
			if err := yaml.Unmarshal(contents, &document); err != nil {
				t.Fatal(err)
			}
			if len(document) != 1 || document["network"] == nil {
				t.Fatal("fresh default config must contain only network")
			}
		})
	}
}

func TestDefaultSavePreservesLatestOpaqueConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte("game_proxy:\n  node:\n    password: stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := loadAppConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	config.FilePath = path
	config.Network.UDPListen = "127.0.0.1:4321"

	// The old proxy's validity and schema are not the default build's concern.
	external := []byte(`network:
  tracker_urls: []
game_proxy:
  directories: ['C:\Games', 'D:\Other Games']
  node:
    server: proxy.example.com
    port: 4567
    username: ' player '
    password: ' new secret with spaces '
    mtu: 1400
    dns: 1.1.1.1
    encrypt: true
    future_option: {nested: [1, false, null, '001']}
  future_section: [{token: keep-me}, {}]
`)
	if err := os.WriteFile(path, external, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]any
	if err := yaml.Unmarshal(external, &before); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(contents, &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after["game_proxy"], before["game_proxy"]) {
		t.Fatal("Save changed the latest opaque config")
	}
	loaded, err := loadAppConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Network, config.Network) {
		t.Fatal("Save did not update network")
	}
	encoded, err := json.Marshal(loaded)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(encoded, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot) != 2 || snapshot["Network"] == nil {
		t.Fatal("opaque config leaked into JSON")
	}

	// Removal by another build must not resurrect a section from the stale load.
	if err := os.WriteFile(path, []byte("network: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(); err != nil {
		t.Fatal(err)
	}
	contents, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	after = nil
	if err := yaml.Unmarshal(contents, &after); err != nil {
		t.Fatal(err)
	}
	if _, exists := after["game_proxy"]; exists {
		t.Fatal("Save resurrected removed opaque config")
	}
}

func TestDefaultSavePreservesOpaqueAliasesAndTags(t *testing.T) {
	for name, original := range map[string]string{
		"network aliases": `network: &network
  udp_listen: &listen '127.0.0.1:7654'
  stun_servers: &servers [stun.example.com:3478]
  port_mapping: &mapping false
game_proxy:
  network_copy: *network
  listen_copy: *listen
  servers_copy: *servers
  mapping_copy: *mapping
  node: &node {password: ' secret ', extension: !future 'opaque'}
  node_copy: *node
  merged: {<<: *network, extra: true}
  binary: !!binary c2VjcmV0
`,
		"whole section alias": "network: &network {udp_listen: '127.0.0.1:7654'}\ngame_proxy: *network\n",
		"explicit null":       "game_proxy: null\n",
		"scalar":              "game_proxy: !future opaque\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			config, err := loadAppConfigFile(path)
			if err != nil {
				t.Fatal(err)
			}
			config.FilePath = path
			config.Network.UDPListen = "127.0.0.1:4321"
			config.Network.STUNServers = []string{}
			config.Network.PortMapping = true
			for range 2 {
				if err := config.Save(); err != nil {
					t.Fatal(err)
				}
				contents, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var before, after map[string]any
				if err := yaml.Unmarshal([]byte(original), &before); err != nil {
					t.Fatal(err)
				}
				if err := yaml.Unmarshal(contents, &after); err != nil {
					t.Fatal(err)
				}
				value, exists := after["game_proxy"]
				if !exists || !reflect.DeepEqual(value, before["game_proxy"]) {
					t.Fatal("Save lost aliased values or opaque section")
				}
				var document struct {
					GameProxy yaml.Node `yaml:"game_proxy"`
				}
				if err := yaml.Unmarshal(contents, &document); err != nil {
					t.Fatal(err)
				}
				if name == "network aliases" {
					var proxy struct {
						Node struct {
							Extension yaml.Node `yaml:"extension"`
						} `yaml:"node"`
						Binary yaml.Node `yaml:"binary"`
					}
					if err := document.GameProxy.Decode(&proxy); err != nil {
						t.Fatal(err)
					}
					if proxy.Node.Extension.Tag != "!future" || proxy.Binary.Tag != "!!binary" {
						t.Fatal("Save lost opaque YAML tags")
					}
				}
			}
		})
	}
}

func TestDefaultSaveRejectsInvalidOpaqueConfig(t *testing.T) {
	for name, original := range map[string]string{
		"duplicate field": "game_proxy:\n  node: {password: one, password: two}\n",
		"missing alias":   "game_proxy: *missing\n",
		"cyclic alias":    "game_proxy: &proxy {self: *proxy}\n",
		"masked cycle":    "game_proxy: {self: safe, <<: &cycle {self: *cycle}}\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			config, err := loadAppConfigFile(path)
			if err != nil {
				t.Fatal(err)
			}
			config.FilePath = path
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadAppConfigFile(path); err == nil {
				t.Fatal("load accepted invalid opaque YAML")
			}
			if err := config.Save(); err == nil {
				t.Fatal("Save accepted invalid opaque YAML")
			}
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(contents) != original {
				t.Fatal("Save overwrote invalid opaque YAML")
			}
		})
	}
}

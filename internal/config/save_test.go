package config

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestEnsureFileDoesNotReplaceExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	original := []byte("network:\n  tracker_urls: []\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := loadAppConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	config.FilePath = path
	config.Network.UDPListen = "127.0.0.1:4321"

	if err := config.EnsureFile(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != string(original) {
		t.Fatalf("contents = %q, want %q", contents, original)
	}
}

func TestSaveCreatesMissingConfigWithPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yml")
	config, err := loadAppConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	config.FilePath = path
	config.Network.UDPListen = "127.0.0.1:4321"

	if err := config.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadAppConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Network, config.Network) {
		t.Fatal("saved network config did not reload with all updated fields")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("permissions = %o", info.Mode().Perm())
		}
	}
}

func TestSaveAtomicallyReplacesWholeExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte("network:\n  tracker_urls: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := loadAppConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	config.FilePath = path
	config.Network.UDPListen = "127.0.0.1:4321"
	config.Network.STUNServers = []string{}
	config.Network.TrackerURLs = []string{"https://tracker.example.com/announce"}
	config.Network.PortMapping = false

	if err := config.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadAppConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Network, config.Network) {
		t.Fatal("saved config did not reload with all updated fields")
	}
}

func TestSaveDoesNotOverwriteInvalidExistingConfig(t *testing.T) {
	for name, original := range map[string]string{
		"syntax":            "network: [\n",
		"unknown root":      "netwrok: {}\n",
		"unknown network":   "network:\n  udp_litsen: '[::]:0'\n",
		"invalid network":   "network:\n  udp_listen: invalid\n",
		"invalid stun":      "network:\n  stun_servers: [invalid]\n",
		"invalid tracker":   "network:\n  tracker_urls: [invalid]\n",
		"duplicate root":    "network: {}\nnetwork: {}\n",
		"duplicate network": "network:\n  port_mapping: true\n  port_mapping: false\n",
		"second document":   "network: {}\n---\nnetwork: {}\n",
		"empty second doc":  "network: {}\n---\n",
		"invalid trailing":  "network: {}\n---\n[\n",
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
				t.Fatal("load accepted an invalid config")
			}
			if err := config.Save(); err == nil {
				t.Fatal("Save accepted an invalid config")
			}
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(contents) != original {
				t.Fatal("Save overwrote an invalid config")
			}
		})
	}
}

func TestSaveRejectsNonRegularTargetWithoutFollowingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require Windows privileges")
	}
	root := t.TempDir()
	target := filepath.Join(root, "target.yml")
	path := filepath.Join(root, "config.yml")
	original := []byte("do not replace")
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	config := AppConfig{FilePath: path}

	if err := config.Save(); err == nil {
		t.Fatal("Save succeeded")
	}
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != string(original) {
		t.Fatalf("target contents = %q, want %q", contents, original)
	}
}

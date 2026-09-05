package config

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
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
	config.GameProxy.Directories = []string{"/changed"}

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
	config.GameProxy.Directories = []string{"/local/games"}

	if err := config.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadAppConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.GameProxy.Directories) != 1 || loaded.GameProxy.Directories[0] != filepath.Clean("/local/games") {
		t.Fatalf("GameProxy.Directories = %q", loaded.GameProxy.Directories)
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
	config.GameProxy = GameProxyConfig{Directories: []string{"/local/games"}, Node: validGameProxyNode()}

	if err := config.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadAppConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Network.UDPListen != "127.0.0.1:4321" || len(loaded.GameProxy.Directories) != 1 || loaded.GameProxy.Directories[0] != filepath.Clean("/local/games") || loaded.GameProxy.Node.Password != "secret" {
		t.Fatal("saved config did not reload with all updated fields")
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

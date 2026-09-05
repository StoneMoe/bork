package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"bork/internal/config"
	"bork/internal/gameproxy"

	"gopkg.in/yaml.v3"
)

func TestSnapshot_projects_saved_config_separately_from_live_status(t *testing.T) {
	manager := newFakeGameProxyManager()
	manager.setStatus(gameproxy.Status{
		Supported: true, State: gameproxy.StateRunning, Generation: 7,
		ExecutableCount: 3, Directories: []string{"/live/games"},
	})
	application := NewApp(config.AppConfig{GameProxy: config.GameProxyConfig{
		Directories: []string{"/saved/games"},
		Node: config.GameProxyNodeConfig{
			Server: "saved.example", Port: 4567, Username: "saved-user",
			Password: " saved-secret ", MTU: 1400, DNS: "1.1.1.1",
		},
	}}, nil)
	application.gameProxyManager = manager

	state := application.snapshot()

	if !reflect.DeepEqual(state.GameProxy.Config.Directories, []string{"/saved/games"}) || state.GameProxy.Config.Node.Server != "saved.example" {
		t.Fatalf("snapshot config = %#v", state.GameProxy.Config)
	}
	if state.GameProxy.Config.Node.Password != " saved-secret " {
		t.Fatalf("snapshot password = %q", state.GameProxy.Config.Node.Password)
	}
	if state.GameProxy.Status.State != string(gameproxy.StateRunning) || !reflect.DeepEqual(state.GameProxy.Status.Directories, []string{"/live/games"}) {
		t.Fatalf("snapshot status = %#v", state.GameProxy.Status)
	}

	manager.setStatus(gameproxy.Status{Supported: true, State: gameproxy.StateFailed, Error: "failed later"})
	next := application.snapshot()
	if !reflect.DeepEqual(next.GameProxy.Config, state.GameProxy.Config) {
		t.Fatalf("saved config changed with manager status: before=%#v after=%#v", state.GameProxy.Config, next.GameProxy.Config)
	}
	if next.GameProxy.Status.State != string(gameproxy.StateFailed) || next.GameProxy.Status.Error != "failed later" {
		t.Fatalf("next snapshot status = %#v", next.GameProxy.Status)
	}
}

func TestSnapshot_projects_empty_game_proxy_collections_as_arrays(t *testing.T) {
	application := NewApp(config.AppConfig{}, nil)

	contents, err := json.Marshal(application.snapshot().GameProxy)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(contents)
	for _, field := range []string{`"directories":[]`, `"events":[]`} {
		if !strings.Contains(encoded, field) {
			t.Fatalf("snapshot JSON = %s, want %s", encoded, field)
		}
	}
}

func TestSaveGameProxyConfig_normalizes_copy_and_preserves_unrelated_config(t *testing.T) {
	root := t.TempDir()
	application := startedGameProxyTestApp(config.AppConfig{
		FilePath: filepath.Join(root, "config.yml"),
		Network: config.NetworkConfig{
			UDPListen: "127.0.0.1:4321", STUNServers: []string{"stun.example:3478"},
			TrackerURLs: []string{}, PortMapping: true,
		},
	})
	// Network configuration is edited on disk, independently of the proxy form.
	if err := os.WriteFile(application.config.FilePath, []byte("network:\n  udp_listen: '127.0.0.1:5432'\n  stun_servers: []\n  tracker_urls: []\n  port_mapping: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	input := validGameProxyConfigInput(filepath.Join(root, "games", "..", "games"))
	secondDirectory := filepath.Join(root, "other-games")
	input.Directories = []string{"  " + input.Directories[0] + "  ", secondDirectory, filepath.Join(root, "games")}
	input.Node.Server = "  proxy.example  "
	input.Node.Username = "  player  "
	input.Node.Password = " secret "
	input.Node.Encrypt = true

	err := application.SaveGameProxyConfig(input)

	if err != nil {
		t.Fatal(err)
	}
	state := application.snapshot()
	if !reflect.DeepEqual(state.GameProxy.Config.Directories, []string{filepath.Join(root, "games"), secondDirectory}) {
		t.Fatalf("saved directories = %q", state.GameProxy.Config.Directories)
	}
	if state.GameProxy.Config.Node.Server != "proxy.example" || state.GameProxy.Config.Node.Username != "player" {
		t.Fatalf("saved node = %#v", state.GameProxy.Config.Node)
	}
	if state.GameProxy.Config.Node.Password != " secret " || !state.GameProxy.Config.Node.Encrypt {
		t.Fatalf("saved password = %q", state.GameProxy.Config.Node.Password)
	}
	if application.config.Network.UDPListen != "127.0.0.1:4321" || !application.config.Network.PortMapping {
		t.Fatalf("unrelated network config changed: %#v", application.config.Network)
	}
	contents, err := os.ReadFile(application.config.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	var saved config.AppConfig
	if err := yaml.Unmarshal(contents, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Network.UDPListen != "127.0.0.1:5432" || saved.Network.PortMapping || len(saved.Network.STUNServers) != 0 || len(saved.Network.TrackerURLs) != 0 {
		t.Fatal("proxy save overwrote external network edits")
	}
}

func TestSaveGameProxyConfig_does_not_mutate_config_when_atomic_save_fails(t *testing.T) {
	original := validConfigGameProxy("/saved/original")
	application := startedGameProxyTestApp(config.AppConfig{
		FilePath:  t.TempDir(),
		Network:   config.NetworkConfig{UDPListen: "127.0.0.1:1234"},
		GameProxy: original,
	})
	input := validGameProxyConfigInput("/replacement")

	err := application.SaveGameProxyConfig(input)

	if err == nil {
		t.Fatal("SaveGameProxyConfig succeeded for a directory target")
	}
	if got := application.snapshot().GameProxy.Config; !reflect.DeepEqual(got, projectGameProxyConfig(original)) {
		t.Fatalf("config after failed save = %#v", got)
	}
	if application.config.Network.UDPListen != "127.0.0.1:1234" {
		t.Fatalf("network config after failed save = %#v", application.config.Network)
	}
}

func TestSaveGameProxyConfig_allows_clearing_directories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	application := startedGameProxyTestApp(config.AppConfig{FilePath: path, GameProxy: validConfigGameProxy("/saved/original")})
	input := validGameProxyConfigInput("ignored")
	input.Directories = []string{"", " \t\n "}

	if err := application.SaveGameProxyConfig(input); err != nil {
		t.Fatal(err)
	}
	if got := application.snapshot().GameProxy.Config.Directories; got == nil || len(got) != 0 {
		t.Fatalf("saved directories = %#v, want empty list", got)
	}
}

func TestSaveGameProxyConfig_updates_directories_while_running(t *testing.T) {
	root := t.TempDir()
	manager := newFakeGameProxyManager()
	manager.setStatus(gameproxy.Status{Supported: true, State: gameproxy.StateRunning, Directories: []string{"C:/old"}})
	var updated []string
	manager.updateDirectoriesFunc = func(_ context.Context, directories []string) error {
		updated = append([]string(nil), directories...)
		return nil
	}
	application := startedGameProxyTestApp(config.AppConfig{
		FilePath: filepath.Join(root, "config.yml"), GameProxy: validConfigGameProxy("C:/old"),
	})
	application.gameProxyManager = manager
	input := validGameProxyConfigInput("C:/new")

	if err := application.SaveGameProxyConfig(input); err != nil {
		t.Fatal(err)
	}
	wantDirectories := []string{filepath.Clean("C:/new")}
	if !reflect.DeepEqual(updated, wantDirectories) {
		t.Fatalf("runtime directories = %q", updated)
	}
	if got := application.snapshot().GameProxy.Config.Directories; !reflect.DeepEqual(got, wantDirectories) {
		t.Fatalf("saved directories = %q", got)
	}
}

func TestSaveGameProxyConfig_rejects_node_changes_while_running(t *testing.T) {
	manager := newFakeGameProxyManager()
	manager.setStatus(gameproxy.Status{Supported: true, State: gameproxy.StateRunning, Directories: []string{"C:/old"}})
	application := startedGameProxyTestApp(config.AppConfig{GameProxy: validConfigGameProxy("C:/old")})
	application.gameProxyManager = manager
	input := validGameProxyConfigInput("C:/new")
	input.Node.Server = "other.example"

	if err := application.SaveGameProxyConfig(input); err == nil || !strings.Contains(err.Error(), "cannot be changed") {
		t.Fatalf("SaveGameProxyConfig error = %v", err)
	}
}

func TestSaveGameProxyConfig_rejects_transitional_states_without_writing(t *testing.T) {
	for _, state := range []gameproxy.State{gameproxy.StateStarting, gameproxy.StateStopping} {
		t.Run(string(state), func(t *testing.T) {
			manager := newFakeGameProxyManager()
			manager.setStatus(gameproxy.Status{Supported: true, State: state})
			path := filepath.Join(t.TempDir(), "config.yml")
			original := []byte("unchanged file")
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			application := startedGameProxyTestApp(config.AppConfig{FilePath: path, GameProxy: validConfigGameProxy("/old")})
			application.gameProxyManager = manager
			input := validGameProxyConfigInput("/new")
			input.Node.Server = "other.example"
			if err := application.SaveGameProxyConfig(input); err == nil || !strings.Contains(err.Error(), "starting or stopping") {
				t.Fatalf("transitional save = %v", err)
			}
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(contents) != string(original) || application.config.GameProxy.Node.Server != "proxy.example" {
				t.Fatal("rejected save changed disk or app configuration")
			}
		})
	}
}

func TestSaveGameProxyConfig_rolls_back_runtime_directories_when_save_fails(t *testing.T) {
	manager := newFakeGameProxyManager()
	oldDirectories := []string{filepath.Clean("C:/old")}
	manager.setStatus(gameproxy.Status{Supported: true, State: gameproxy.StateRunning, Directories: oldDirectories})
	var updates [][]string
	manager.updateDirectoriesFunc = func(_ context.Context, directories []string) error {
		updates = append(updates, append([]string(nil), directories...))
		return nil
	}
	application := startedGameProxyTestApp(config.AppConfig{
		FilePath: t.TempDir(), GameProxy: validConfigGameProxy(oldDirectories[0]),
	})
	application.gameProxyManager = manager

	err := application.SaveGameProxyConfig(validGameProxyConfigInput("C:/new"))
	if err == nil {
		t.Fatal("SaveGameProxyConfig succeeded for a directory target")
	}
	wantUpdates := [][]string{{filepath.Clean("C:/new")}, oldDirectories}
	if !reflect.DeepEqual(updates, wantUpdates) {
		t.Fatalf("runtime updates = %q, want update then rollback %q", updates, wantUpdates)
	}
	if got := application.snapshot().GameProxy.Config.Directories; !reflect.DeepEqual(got, oldDirectories) {
		t.Fatalf("config after failed save = %q", got)
	}
}

func TestSaveGameProxyConfig_stops_proxy_if_persistence_and_rollback_fail(t *testing.T) {
	manager := newFakeGameProxyManager()
	oldDirectories := []string{filepath.Clean("/old")}
	manager.setStatus(gameproxy.Status{Supported: true, State: gameproxy.StateRunning, Directories: oldDirectories})
	rollbackErr := errors.New("old directory disappeared")
	updates := 0
	manager.updateDirectoriesFunc = func(_ context.Context, directories []string) error {
		updates++
		if updates == 2 {
			return rollbackErr
		}
		manager.setStatus(gameproxy.Status{Supported: true, State: gameproxy.StateRunning, Directories: directories})
		return nil
	}
	application := startedGameProxyTestApp(config.AppConfig{FilePath: t.TempDir(), GameProxy: validConfigGameProxy(oldDirectories[0])})
	application.gameProxyManager = manager
	stopped := false
	manager.stopFunc = func() {
		stopped = true
		if !application.commandMu.TryLock() {
			t.Error("rollback failure stopped proxy while holding commandMu")
		} else {
			application.commandMu.Unlock()
		}
		manager.setStatus(gameproxy.Status{Supported: true, State: gameproxy.StateInactive})
	}
	if err := application.SaveGameProxyConfig(validGameProxyConfigInput("/new")); !errors.Is(err, rollbackErr) || !strings.Contains(err.Error(), "game proxy stopped") {
		t.Fatalf("failed rollback = %v", err)
	}
	if !stopped || manager.Status().State != gameproxy.StateInactive || !reflect.DeepEqual(application.config.GameProxy.Directories, oldDirectories) {
		t.Fatal("failed save left rejected rules running or changed saved configuration")
	}
}

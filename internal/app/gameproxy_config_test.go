package app

import (
	"os"
	"path/filepath"
	"testing"

	"bork/internal/config"
	"bork/internal/gameproxy"
)

func TestSnapshot_projects_saved_config_separately_from_live_status(t *testing.T) {
	manager := newFakeGameProxyManager()
	manager.setStatus(gameproxy.Status{
		Supported: true, State: gameproxy.StateRunning, Generation: 7,
		ExecutableCount: 3, Directory: "/live/games",
	})
	application := NewApp(config.AppConfig{GameProxy: config.GameProxyConfig{
		Directory: "/saved/games",
		Node: config.GameProxyNodeConfig{
			Server: "saved.example", Port: 4567, Username: "saved-user",
			Password: " saved-secret ", MTU: 1400, DNS: "1.1.1.1",
		},
	}}, nil)
	application.gameProxyManager = manager

	state := application.snapshot()

	if state.GameProxy.Config.Directory != "/saved/games" || state.GameProxy.Config.Node.Server != "saved.example" {
		t.Fatalf("snapshot config = %#v", state.GameProxy.Config)
	}
	if state.GameProxy.Config.Node.Password != " saved-secret " {
		t.Fatalf("snapshot password = %q", state.GameProxy.Config.Node.Password)
	}
	if state.GameProxy.Status.State != string(gameproxy.StateRunning) || state.GameProxy.Status.Directory != "/live/games" {
		t.Fatalf("snapshot status = %#v", state.GameProxy.Status)
	}

	manager.setStatus(gameproxy.Status{Supported: true, State: gameproxy.StateFailed, Error: "failed later"})
	next := application.snapshot()
	if next.GameProxy.Config != state.GameProxy.Config {
		t.Fatalf("saved config changed with manager status: before=%#v after=%#v", state.GameProxy.Config, next.GameProxy.Config)
	}
	if next.GameProxy.Status.State != string(gameproxy.StateFailed) || next.GameProxy.Status.Error != "failed later" {
		t.Fatalf("next snapshot status = %#v", next.GameProxy.Status)
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
	input := validGameProxyConfigInput(filepath.Join(root, "games", "..", "games"))
	input.Directory = "  " + input.Directory + "  "
	input.Node.Server = "  proxy.example  "
	input.Node.Username = "  player  "
	input.Node.Password = " secret "

	err := application.SaveGameProxyConfig(input)

	if err != nil {
		t.Fatal(err)
	}
	state := application.snapshot()
	if state.GameProxy.Config.Directory != filepath.Join(root, "games") {
		t.Fatalf("saved directory = %q", state.GameProxy.Config.Directory)
	}
	if state.GameProxy.Config.Node.Server != "proxy.example" || state.GameProxy.Config.Node.Username != "player" {
		t.Fatalf("saved node = %#v", state.GameProxy.Config.Node)
	}
	if state.GameProxy.Config.Node.Password != " secret " {
		t.Fatalf("saved password = %q", state.GameProxy.Config.Node.Password)
	}
	if application.config.Network.UDPListen != "127.0.0.1:4321" || !application.config.Network.PortMapping {
		t.Fatalf("unrelated network config changed: %#v", application.config.Network)
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
	if got := application.snapshot().GameProxy.Config; got != projectGameProxyConfig(original) {
		t.Fatalf("config after failed save = %#v", got)
	}
	if application.config.Network.UDPListen != "127.0.0.1:1234" {
		t.Fatalf("network config after failed save = %#v", application.config.Network)
	}
}

func TestSaveGameProxyConfig_rejects_empty_directory_without_mutation(t *testing.T) {
	for _, directory := range []string{"", " \t\n "} {
		t.Run(directory, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			originalContents := []byte("unchanged config contents")
			if err := os.WriteFile(path, originalContents, 0o600); err != nil {
				t.Fatal(err)
			}
			original := validConfigGameProxy("/saved/original")
			application := startedGameProxyTestApp(config.AppConfig{FilePath: path, GameProxy: original})
			input := validGameProxyConfigInput(directory)

			err := application.SaveGameProxyConfig(input)

			if err == nil {
				t.Fatal("SaveGameProxyConfig accepted an empty directory")
			}
			contents, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(contents) != string(originalContents) {
				t.Fatalf("disk config changed to %q", contents)
			}
			if got := application.snapshot().GameProxy.Config; got != projectGameProxyConfig(original) {
				t.Fatalf("memory config changed to %#v", got)
			}
		})
	}
}

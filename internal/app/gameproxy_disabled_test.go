//go:build !game_proxy

package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"bork/internal/config"
)

func TestDefaultApp_has_no_game_proxy_RPCs(t *testing.T) {
	appType := reflect.TypeFor[*App]()
	for index := 0; index < appType.NumMethod(); index++ {
		if method := appType.Method(index); strings.Contains(method.Name, "GameProxy") {
			t.Errorf("default App exposes %s", method.Name)
		}
	}
}

func TestDefaultApp_snapshot_excludes_game_proxy(t *testing.T) {
	application := NewApp(config.AppConfig{}, nil)
	state := application.snapshot()
	if reflect.TypeOf(state).NumField() != 5 {
		t.Fatal("default snapshot must contain only the five common fields")
	}
	if state.Room != nil {
		t.Fatal("new app has a room")
	}
	for _, room := range []*RoomState{nil, {Name: "test room"}} {
		state.Room = room
		contents, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(contents, &fields); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"version", "nickname", "audio", "diagnostics"} {
			if _, ok := fields[name]; !ok {
				t.Errorf("snapshot JSON is missing %s", name)
			}
			delete(fields, name)
		}
		if _, ok := fields["room"]; ok != (room != nil) {
			t.Error("snapshot JSON did not preserve optional room")
		}
		delete(fields, "room")
		if len(fields) != 0 {
			t.Errorf("default snapshot JSON has feature fields: %v", fields)
		}
	}
}

func TestDefaultApp_proxy_hooks_have_no_side_effects(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"LOCALAPPDATA", "APPDATA", "XDG_CONFIG_HOME", "HOME"} {
		t.Setenv(name, root)
	}
	cfg := config.AppConfig{FilePath: filepath.Join(root, "config.yml")}
	application := NewApp(cfg, nil)
	stateType := reflect.TypeOf(application.gameProxyAppState)
	if stateType.NumField() != 0 || stateType.Size() != 0 {
		t.Fatal("default app retained proxy runtime state")
	}
	application.stateMu.Lock()
	application.appContext = t.Context()
	application.initGameProxyRunContextLocked(t.Context())
	cancel := application.detachGameProxyRunContextLocked()
	application.stateMu.Unlock()
	if cancel != nil {
		t.Fatal("default hook created a proxy run context")
	}
	application.startGameProxyWatcher(t.Context())
	close(application.startupDone)
	_ = application.GetSnapshot()
	application.shutdown(t.Context())
	application.shutdown(t.Context())
	select {
	case <-application.statePending:
		t.Fatal("default proxy hooks queued a state change")
	default:
	}
	if !reflect.DeepEqual(application.config, cfg) {
		t.Fatal("default proxy hooks changed app configuration")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("default app created files: %v", entries)
	}
}

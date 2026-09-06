//go:build game_proxy

package app

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"bork/internal/config"
	"bork/internal/gameproxy"
	"bork/internal/gameproxy/iwan"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

type fakeGameProxyManager struct {
	startFunc             func(context.Context, gameproxy.StartInput) error
	stopFunc              func()
	updateDirectoriesFunc func(context.Context, []string) error
	changes               chan struct{}

	statusMu sync.Mutex
	status   gameproxy.Status
}

func newFakeGameProxyManager() *fakeGameProxyManager {
	return &fakeGameProxyManager{changes: make(chan struct{}, 1)}
}

func (manager *fakeGameProxyManager) Start(ctx context.Context, input gameproxy.StartInput) error {
	if manager.startFunc == nil {
		return nil
	}
	return manager.startFunc(ctx, input)
}

func (manager *fakeGameProxyManager) Stop() {
	if manager.stopFunc != nil {
		manager.stopFunc()
	}
}

func (manager *fakeGameProxyManager) UpdateDirectories(ctx context.Context, directories []string) error {
	if manager.updateDirectoriesFunc == nil {
		return nil
	}
	return manager.updateDirectoriesFunc(ctx, directories)
}

func (manager *fakeGameProxyManager) Status() gameproxy.Status {
	manager.statusMu.Lock()
	defer manager.statusMu.Unlock()
	return manager.status
}

func (manager *fakeGameProxyManager) Changes() <-chan struct{} { return manager.changes }

func (manager *fakeGameProxyManager) setStatus(status gameproxy.Status) {
	manager.statusMu.Lock()
	manager.status = status
	manager.statusMu.Unlock()
}

func TestSelectGameProxyDirectory_opens_dialog_without_app_locks(t *testing.T) {
	directory := t.TempDir()
	application := startedGameProxyTestApp(config.AppConfig{GameProxy: validConfigGameProxy(directory)})
	entered := make(chan wailsruntime.OpenDialogOptions, 1)
	release := make(chan struct{})
	application.selectGameProxyDirectory = func(_ context.Context, options wailsruntime.OpenDialogOptions) (string, error) {
		entered <- options
		<-release
		return directory, nil
	}
	result := make(chan string, 1)
	resultErr := make(chan error, 1)
	go func() {
		selected, err := application.SelectGameProxyDirectory()
		result <- selected
		resultErr <- err
	}()

	options := receiveTestValue(t, entered)
	if !application.commandMu.TryLock() {
		t.Fatal("directory dialog held commandMu")
	}
	application.commandMu.Unlock()
	if !application.stateMu.TryLock() {
		t.Fatal("directory dialog held stateMu")
	}
	application.stateMu.Unlock()
	close(release)

	if selected := receiveTestValue(t, result); selected != directory {
		t.Fatalf("selected directory = %q", selected)
	}
	if err := receiveTestValue(t, resultErr); err != nil {
		t.Fatal(err)
	}
	if options.DefaultDirectory != directory {
		t.Fatalf("directory dialog options = %#v", options)
	}
}

func TestSelectGameProxyDirectory_omits_missing_saved_default(t *testing.T) {
	application := startedGameProxyTestApp(config.AppConfig{GameProxy: validConfigGameProxy(filepath.Join(t.TempDir(), "missing"))})
	var options wailsruntime.OpenDialogOptions
	application.selectGameProxyDirectory = func(_ context.Context, value wailsruntime.OpenDialogOptions) (string, error) {
		options = value
		return "", nil
	}

	_, err := application.SelectGameProxyDirectory()

	if err != nil {
		t.Fatal(err)
	}
	if options.DefaultDirectory != "" {
		t.Fatalf("missing default directory = %q", options.DefaultDirectory)
	}
}

func TestStartGameProxy_maps_saved_config_before_returning(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(t.Context(), contextKey{}, "app")
	manager := newFakeGameProxyManager()
	startInput := make(chan gameproxy.StartInput, 1)
	startContext := make(chan context.Context, 1)
	startDone := make(chan struct{})
	manager.startFunc = func(startCtx context.Context, input gameproxy.StartInput) error {
		if startCtx.Value(contextKey{}) != "app" {
			t.Errorf("start context did not derive from app context")
		}
		startContext <- startCtx
		startInput <- input
		<-startCtx.Done()
		close(startDone)
		return startCtx.Err()
	}
	stored := validConfigGameProxy(" /games/../games ")
	stored.Node.Server = " proxy.example "
	stored.Node.Username = " player "
	stored.Node.Password = " secret "
	stored.Node.Encrypt = true
	application := startedGameProxyTestAppWithContext(config.AppConfig{GameProxy: stored}, ctx)
	application.gameProxyManager = manager

	err := application.StartGameProxy()

	if err != nil {
		t.Fatal(err)
	}
	input := receiveTestValue(t, startInput)
	runCtx := receiveTestValue(t, startContext)
	wantNode := iwan.Node{
		Server: "proxy.example", Port: 4567, Username: "player",
		Password: " secret ", MTU: 1400, Encrypt: true,
	}
	if input.Node != wantNode || !slices.Equal(input.Directories, []string{filepath.Clean("/games")}) || input.DNS != netip.MustParseAddr("1.1.1.1") {
		t.Fatalf("manager start input = %#v", input)
	}
	application.StopGameProxy()
	select {
	case <-runCtx.Done():
	default:
		t.Fatal("StopGameProxy did not cancel the queued start context")
	}
	if application.gameProxyRunContext.Err() != nil {
		t.Fatal("StopGameProxy did not prepare a fresh context for a later start")
	}
	receiveTestSignal(t, startDone)
}

func TestStartGameProxy_rejects_invalid_saved_config_before_launch(t *testing.T) {
	manager := newFakeGameProxyManager()
	started := make(chan struct{}, 1)
	manager.startFunc = func(context.Context, gameproxy.StartInput) error {
		started <- struct{}{}
		return nil
	}
	stored := validConfigGameProxy("/games")
	stored.Node.Password = ""
	application := startedGameProxyTestApp(config.AppConfig{GameProxy: stored})
	application.gameProxyManager = manager

	err := application.StartGameProxy()

	if err == nil {
		t.Fatal("StartGameProxy accepted an empty password")
	}
	select {
	case <-started:
		t.Fatal("manager Start was launched for invalid config")
	default:
	}
}

func TestStartGameProxy_rejects_empty_saved_directories_before_launch(t *testing.T) {
	manager := newFakeGameProxyManager()
	started := make(chan struct{}, 1)
	manager.startFunc = func(context.Context, gameproxy.StartInput) error {
		started <- struct{}{}
		return nil
	}
	stored := validConfigGameProxy("ignored")
	stored.Directories = []string{}
	application := startedGameProxyTestApp(config.AppConfig{GameProxy: stored})
	application.gameProxyManager = manager

	if err := application.StartGameProxy(); err == nil {
		t.Fatal("StartGameProxy accepted an empty directory list")
	}
	select {
	case <-started:
		t.Fatal("manager Start was launched without game directories")
	default:
	}
}

func TestStopGameProxy_calls_manager_without_app_locks(t *testing.T) {
	manager := newFakeGameProxyManager()
	entered := make(chan struct{})
	release := make(chan struct{})
	manager.stopFunc = func() {
		close(entered)
		<-release
	}
	application := startedGameProxyTestApp(config.AppConfig{})
	application.gameProxyManager = manager
	done := make(chan struct{})
	go func() {
		application.StopGameProxy()
		close(done)
	}()
	receiveTestSignal(t, entered)

	if !application.commandMu.TryLock() {
		t.Fatal("manager Stop held commandMu")
	}
	application.commandMu.Unlock()
	if !application.stateMu.TryLock() {
		t.Fatal("manager Stop held stateMu")
	}
	application.stateMu.Unlock()
	close(release)
	receiveTestSignal(t, done)
}

func TestExportGameProxyLogs_writes_all_events(t *testing.T) {
	manager := newFakeGameProxyManager()
	manager.setStatus(gameproxy.Status{Events: []gameproxy.ConnectionEvent{
		{At: "2026-08-31T12:00:00Z", Level: "fatal", Message: "connection failed"},
		{At: "2026-08-31T12:00:01Z", Level: "info", Message: "retrying"},
	}})
	application := startedGameProxyTestApp(config.AppConfig{})
	application.gameProxyManager = manager
	path := filepath.Join(t.TempDir(), "proxy.log")
	application.saveFileDialog = func(context.Context, wailsruntime.SaveDialogOptions) (string, error) { return path, nil }

	if err := application.ExportGameProxyLogs(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "2026-08-31T12:00:00Z [FATAL] connection failed\n2026-08-31T12:00:01Z [INFO] retrying\n"
	if string(contents) != want {
		t.Fatalf("exported log = %q, want %q", contents, want)
	}
}

func startedGameProxyTestApp(cfg config.AppConfig) *App {
	return startedGameProxyTestAppWithContext(cfg, context.Background())
}

func startedGameProxyTestAppWithContext(cfg config.AppConfig, ctx context.Context) *App {
	application := NewApp(cfg, nil)
	application.stateMu.Lock()
	application.appContext = ctx
	application.initGameProxyRunContextLocked(ctx)
	application.stateMu.Unlock()
	close(application.startupDone)
	return application
}

func validGameProxyConfigInput(directory string) GameProxyConfigInput {
	return GameProxyConfigInput{
		Directories: []string{directory},
		Node: GameProxyNodeInput{
			Server: "proxy.example", Port: 4567, Username: "player",
			Password: "secret", MTU: 1400, DNS: "1.1.1.1",
		},
	}
}

func validConfigGameProxy(directory string) config.GameProxyConfig {
	return config.GameProxyConfig{
		Directories: []string{directory},
		Node: config.GameProxyNodeConfig{
			Server: "proxy.example", Port: 4567, Username: "player",
			Password: "secret", MTU: 1400, DNS: "1.1.1.1",
		},
	}
}

func receiveTestSignal(t *testing.T, channel <-chan struct{}) {
	t.Helper()
	receiveTestValue(t, channel)
}

func receiveTestValue[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	select {
	case value := <-channel:
		return value
	case <-ctx.Done():
		t.Fatalf("waiting for test signal: %v", ctx.Err())
		var zero T
		return zero
	}
}

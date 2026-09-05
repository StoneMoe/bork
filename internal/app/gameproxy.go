package app

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"bork/internal/config"
	"bork/internal/gameproxy"
	"bork/internal/gameproxy/iwan"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

type gameProxyManager interface {
	Start(context.Context, gameproxy.StartInput) error
	Stop()
	UpdateDirectories(context.Context, []string) error
	Status() gameproxy.Status
	Changes() <-chan struct{}
}

type GameProxyNodeInput struct {
	Server   string `json:"server"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	MTU      int    `json:"mtu"`
	DNS      string `json:"dns"`
	Encrypt  bool   `json:"encrypt"`
}

type GameProxyConfigInput struct {
	Directories []string           `json:"directories"`
	Node        GameProxyNodeInput `json:"node"`
}

type GameProxyStatusSnapshot struct {
	Supported       bool                        `json:"supported"`
	State           string                      `json:"state"`
	Generation      uint64                      `json:"generation"`
	ExecutableCount int                         `json:"executableCount"`
	Directories     []string                    `json:"directories"`
	Events          []gameproxy.ConnectionEvent `json:"events"`
	Error           string                      `json:"error,omitempty"`
	Traffic         gameproxy.TrafficStats      `json:"traffic"`
}

type GameProxySnapshot struct {
	Config GameProxyConfigInput    `json:"config"`
	Status GameProxyStatusSnapshot `json:"status"`
}

func (a *App) SelectGameProxyDirectory() (string, error) {
	a.waitForStartup()
	a.stateMu.RLock()
	ctx := a.appContext
	shuttingDown := a.shuttingDown
	directories := slices.Clone(a.config.GameProxy.Directories)
	a.stateMu.RUnlock()
	if shuttingDown {
		return "", errors.New("application is shutting down")
	}

	options := wailsruntime.OpenDialogOptions{
		Title: "选择游戏目录",
	}
	for _, directory := range directories {
		if info, err := os.Stat(directory); err == nil && info.IsDir() {
			options.DefaultDirectory = directory
			break
		}
	}
	return a.selectGameProxyDirectory(ctx, options)
}

func (a *App) SaveGameProxyConfig(input GameProxyConfigInput) error {
	a.waitForStartup()
	normalized, err := normalizeGameProxyConfigInput(input)
	if err != nil {
		return err
	}

	a.gameProxyMu.Lock()
	defer a.gameProxyMu.Unlock()
	a.stateMu.RLock()
	runContext := a.gameProxyRunContext
	shuttingDown := a.shuttingDown
	stored := a.config
	a.stateMu.RUnlock()
	if shuttingDown {
		return errors.New("application is shutting down")
	}
	if err := runContext.Err(); err != nil {
		return err
	}
	if a.gameProxyStartDone != nil {
		select {
		case <-a.gameProxyStartDone:
		default:
			return errors.New("game proxy is starting")
		}
	}
	managerStatus := a.gameProxyManager.Status()
	if managerStatus.State == gameproxy.StateStarting || managerStatus.State == gameproxy.StateStopping {
		return errors.New("game proxy settings cannot be changed while starting or stopping")
	}
	active := managerStatus.State == gameproxy.StateRunning || managerStatus.State == gameproxy.StateReconnecting
	candidate := config.GameProxyConfig{
		Directories: slices.Clone(normalized.Directories),
		Node: config.GameProxyNodeConfig{
			Server: normalized.Node.Server, Port: normalized.Node.Port,
			Username: normalized.Node.Username, Password: normalized.Node.Password,
			MTU: normalized.Node.MTU, DNS: normalized.Node.DNS, Encrypt: normalized.Node.Encrypt,
		},
	}
	if active && stored.GameProxy.Node != candidate.Node {
		return errors.New("iWAN settings cannot be changed while the game proxy is running")
	}
	oldDirectories := slices.Clone(managerStatus.Directories)
	directoriesChanged := active && !slices.Equal(oldDirectories, normalized.Directories)
	if directoriesChanged {
		if err := a.gameProxyManager.UpdateDirectories(runContext, normalized.Directories); err != nil {
			return fmt.Errorf("update running game directories: %w", err)
		}
	}
	if err := stored.SaveGameProxy(candidate); err != nil {
		var rollbackErr error
		if directoriesChanged {
			if rollback := a.gameProxyManager.UpdateDirectories(runContext, oldDirectories); rollback != nil {
				a.stopGameProxyLocked()
				rollbackErr = fmt.Errorf("game proxy stopped because restoring game directories failed: %w", rollback)
			}
		}
		return errors.Join(fmt.Errorf("save game proxy config: %w", err), rollbackErr)
	}
	a.stateMu.Lock()
	a.config.GameProxy = candidate
	a.stateMu.Unlock()
	a.markStateChanged()
	return nil
}

func (a *App) StartGameProxy() error {
	a.waitForStartup()
	a.gameProxyMu.Lock()
	defer a.gameProxyMu.Unlock()
	a.stateMu.RLock()
	ctx := a.gameProxyRunContext
	shuttingDown := a.shuttingDown
	stored := a.config.GameProxy
	a.stateMu.RUnlock()
	if shuttingDown {
		return errors.New("application is shutting down")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.gameProxyStartDone != nil {
		select {
		case <-a.gameProxyStartDone:
		default:
			return gameproxy.ErrActive
		}
	}
	input, err := startInputFromConfig(stored)
	if err != nil {
		return err
	}
	manager := a.gameProxyManager
	done := make(chan struct{})
	a.gameProxyStartDone = done
	go func() {
		defer close(done)
		if ctx.Err() == nil {
			_ = manager.Start(ctx, input)
		}
	}()
	return nil
}

func (a *App) StopGameProxy() {
	a.waitForStartup()
	// Cancel before waiting for a save's native update to release gameProxyMu.
	a.stateMu.RLock()
	cancel := a.cancelGameProxyRuns
	a.stateMu.RUnlock()
	if cancel != nil {
		cancel()
	}
	a.gameProxyMu.Lock()
	defer a.gameProxyMu.Unlock()
	a.stopGameProxyLocked()
}

func (a *App) stopGameProxyLocked() {
	a.stateMu.RLock()
	cancel := a.cancelGameProxyRuns
	a.stateMu.RUnlock()
	if cancel != nil {
		cancel()
	}
	// Drain even an unaccepted Start before the final Stop, so it cannot start later.
	if a.gameProxyStartDone != nil {
		<-a.gameProxyStartDone
		a.gameProxyStartDone = nil
	}
	a.gameProxyManager.Stop()
	a.stateMu.Lock()
	if !a.shuttingDown {
		a.gameProxyRunContext, a.cancelGameProxyRuns = context.WithCancel(a.appContext)
	}
	a.stateMu.Unlock()
}

func (a *App) ExportGameProxyLogs() error {
	a.waitForStartup()
	a.stateMu.RLock()
	ctx := a.appContext
	shuttingDown := a.shuttingDown
	a.stateMu.RUnlock()
	if shuttingDown {
		return errors.New("application is shutting down")
	}
	path, err := a.saveFileDialog(ctx, wailsruntime.SaveDialogOptions{
		Title: "导出游戏代理日志", DefaultFilename: "bork-game-proxy-" + time.Now().Format("20060102-150405") + ".log",
	})
	if err != nil || path == "" {
		return err
	}
	status := a.gameProxyManager.Status()
	var contents strings.Builder
	for _, event := range status.Events {
		fmt.Fprintf(&contents, "%s [%s] %s\n", event.At, strings.ToUpper(event.Level), event.Message)
	}
	if err := os.WriteFile(path, []byte(contents.String()), 0o600); err != nil {
		return fmt.Errorf("write game proxy log: %w", err)
	}
	return nil
}

func (a *App) startGameProxyWatcher(parent context.Context) {
	a.stateMu.Lock()
	if a.shuttingDown || a.stopGameProxyWatcherFunc != nil {
		a.stateMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	manager := a.gameProxyManager
	a.stopGameProxyWatcherFunc = cancel
	a.gameProxyWatcherDone = done
	a.stateMu.Unlock()
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-manager.Changes():
				a.markStateChanged()
			}
		}
	}()
}

func (a *App) stopGameProxyWatcher() {
	a.stateMu.Lock()
	stop, done := a.stopGameProxyWatcherFunc, a.gameProxyWatcherDone
	a.stopGameProxyWatcherFunc, a.gameProxyWatcherDone = nil, nil
	a.stateMu.Unlock()
	if stop != nil {
		stop()
	}
	if done != nil {
		<-done
	}
}

func normalizeGameProxyConfigInput(input GameProxyConfigInput) (GameProxyConfigInput, error) {
	node, err := config.ValidateGameProxyNode(config.GameProxyNodeConfig{
		Server: input.Node.Server, Port: input.Node.Port,
		Username: input.Node.Username, Password: input.Node.Password,
		MTU: input.Node.MTU, DNS: input.Node.DNS, Encrypt: input.Node.Encrypt,
	})
	if err != nil {
		return GameProxyConfigInput{}, fmt.Errorf("validate game proxy config: %w", err)
	}
	directories := make([]string, 0, len(input.Directories))
	for _, directory := range input.Directories {
		directory = strings.TrimSpace(directory)
		if directory == "" {
			continue
		}
		directory = filepath.Clean(directory)
		if !slices.Contains(directories, directory) {
			directories = append(directories, directory)
		}
	}
	return projectGameProxyConfig(config.GameProxyConfig{Directories: directories, Node: node}), nil
}

func startInputFromConfig(value config.GameProxyConfig) (gameproxy.StartInput, error) {
	normalized, err := normalizeGameProxyConfigInput(projectGameProxyConfig(value))
	if err != nil {
		return gameproxy.StartInput{}, err
	}
	if len(normalized.Directories) == 0 {
		return gameproxy.StartInput{}, errors.New("at least one game proxy directory is required")
	}
	dns, err := netip.ParseAddr(normalized.Node.DNS)
	if err != nil {
		return gameproxy.StartInput{}, fmt.Errorf("parse game proxy DNS: %w", err)
	}
	return gameproxy.StartInput{
		Directories: slices.Clone(normalized.Directories),
		DNS:         dns,
		Node: iwan.Node{
			Server: normalized.Node.Server, Port: uint16(normalized.Node.Port),
			Username: normalized.Node.Username, Password: normalized.Node.Password,
			MTU: uint16(normalized.Node.MTU), Encrypt: normalized.Node.Encrypt,
		},
	}, nil
}

func projectGameProxyConfig(value config.GameProxyConfig) GameProxyConfigInput {
	directories := slices.Clone(value.Directories)
	if directories == nil {
		directories = []string{}
	}
	return GameProxyConfigInput{
		Directories: directories,
		Node: GameProxyNodeInput{
			Server: value.Node.Server, Port: value.Node.Port,
			Username: value.Node.Username, Password: value.Node.Password,
			MTU: value.Node.MTU, DNS: value.Node.DNS, Encrypt: value.Node.Encrypt,
		},
	}
}

func projectGameProxyStatus(value gameproxy.Status) GameProxyStatusSnapshot {
	directories := slices.Clone(value.Directories)
	if directories == nil {
		directories = []string{}
	}
	events := slices.Clone(value.Events)
	if events == nil {
		events = []gameproxy.ConnectionEvent{}
	}
	return GameProxyStatusSnapshot{
		Supported: value.Supported, State: string(value.State), Generation: value.Generation,
		ExecutableCount: value.ExecutableCount, Directories: directories,
		Events: events, Error: value.Error, Traffic: value.Traffic,
	}
}

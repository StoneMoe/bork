package app

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"bork/internal/config"
	"bork/internal/gameproxy"
	"bork/internal/gameproxy/iwan"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

type gameProxyManager interface {
	Start(context.Context, gameproxy.StartInput) error
	Stop()
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
}

type GameProxyConfigInput struct {
	Directory string             `json:"directory"`
	Node      GameProxyNodeInput `json:"node"`
}

type GameProxyStatusSnapshot struct {
	Supported       bool   `json:"supported"`
	State           string `json:"state"`
	Generation      uint64 `json:"generation"`
	ExecutableCount int    `json:"executableCount"`
	Directory       string `json:"directory"`
	Error           string `json:"error,omitempty"`
}

type GameProxySnapshot struct {
	Config GameProxyConfigInput    `json:"config"`
	Status GameProxyStatusSnapshot `json:"status"`
}

func (a *App) SelectGameProxyDirectory() (string, error) {
	a.waitForStartup()
	a.commandMu.Lock()
	a.stateMu.RLock()
	ctx := a.appContext
	shuttingDown := a.shuttingDown
	a.stateMu.RUnlock()
	directory := a.config.GameProxy.Directory
	a.commandMu.Unlock()
	if shuttingDown {
		return "", errors.New("application is shutting down")
	}

	options := wailsruntime.OpenDialogOptions{
		Title: "选择游戏目录",
	}
	if info, err := os.Stat(directory); err == nil && info.IsDir() {
		options.DefaultDirectory = directory
	}
	return a.selectGameProxyDirectory(ctx, options)
}

func (a *App) SaveGameProxyConfig(input GameProxyConfigInput) error {
	a.waitForStartup()
	normalized, err := normalizeGameProxyConfigInput(input)
	if err != nil {
		return err
	}

	a.commandMu.Lock()
	a.stateMu.RLock()
	shuttingDown := a.shuttingDown
	a.stateMu.RUnlock()
	if shuttingDown {
		a.commandMu.Unlock()
		return errors.New("application is shutting down")
	}
	candidate := a.config
	candidate.GameProxy = config.GameProxyConfig{
		Directory: normalized.Directory,
		Node: config.GameProxyNodeConfig{
			Server: normalized.Node.Server, Port: normalized.Node.Port,
			Username: normalized.Node.Username, Password: normalized.Node.Password,
			MTU: normalized.Node.MTU, DNS: normalized.Node.DNS,
		},
	}
	if err := candidate.Save(); err != nil {
		a.commandMu.Unlock()
		return fmt.Errorf("save game proxy config: %w", err)
	}
	a.stateMu.Lock()
	a.config = candidate
	a.stateMu.Unlock()
	a.commandMu.Unlock()
	a.markStateChanged()
	return nil
}

func (a *App) StartGameProxy() error {
	a.waitForStartup()
	a.commandMu.Lock()
	a.stateMu.RLock()
	ctx := a.gameProxyRunContext
	shuttingDown := a.shuttingDown
	a.stateMu.RUnlock()
	if shuttingDown {
		a.commandMu.Unlock()
		return errors.New("application is shutting down")
	}
	input, err := startInputFromConfig(a.config.GameProxy)
	manager := a.gameProxyManager
	a.commandMu.Unlock()
	if err != nil {
		return err
	}
	go func() {
		_ = manager.Start(ctx, input)
	}()
	return nil
}

func (a *App) StopGameProxy() {
	a.waitForStartup()
	a.gameProxyManager.Stop()
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
		MTU: input.Node.MTU, DNS: input.Node.DNS,
	})
	if err != nil {
		return GameProxyConfigInput{}, fmt.Errorf("validate game proxy config: %w", err)
	}
	directory := strings.TrimSpace(input.Directory)
	if directory == "" {
		return GameProxyConfigInput{}, errors.New("game proxy directory must not be empty")
	}
	directory = filepath.Clean(directory)
	return projectGameProxyConfig(config.GameProxyConfig{Directory: directory, Node: node}), nil
}

func startInputFromConfig(value config.GameProxyConfig) (gameproxy.StartInput, error) {
	normalized, err := normalizeGameProxyConfigInput(projectGameProxyConfig(value))
	if err != nil {
		return gameproxy.StartInput{}, err
	}
	dns, err := netip.ParseAddr(normalized.Node.DNS)
	if err != nil {
		return gameproxy.StartInput{}, fmt.Errorf("parse game proxy DNS: %w", err)
	}
	return gameproxy.StartInput{
		Directory: normalized.Directory,
		DNS:       dns,
		Node: iwan.Node{
			Server: normalized.Node.Server, Port: uint16(normalized.Node.Port),
			Username: normalized.Node.Username, Password: normalized.Node.Password,
			MTU: uint16(normalized.Node.MTU),
		},
	}, nil
}

func projectGameProxyConfig(value config.GameProxyConfig) GameProxyConfigInput {
	return GameProxyConfigInput{
		Directory: value.Directory,
		Node: GameProxyNodeInput{
			Server: value.Node.Server, Port: value.Node.Port,
			Username: value.Node.Username, Password: value.Node.Password,
			MTU: value.Node.MTU, DNS: value.Node.DNS,
		},
	}
}

func projectGameProxyStatus(value gameproxy.Status) GameProxyStatusSnapshot {
	return GameProxyStatusSnapshot{
		Supported: value.Supported, State: string(value.State), Generation: value.Generation,
		ExecutableCount: value.ExecutableCount, Directory: value.Directory, Error: value.Error,
	}
}

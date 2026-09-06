//go:build !game_proxy

package app

import "context"

type gameProxyAppState struct{}

func newGameProxyAppState() gameProxyAppState { return gameProxyAppState{} }

func (a *App) initGameProxyRunContextLocked(context.Context) {}

func (a *App) startGameProxyWatcher(context.Context) {}

func (a *App) snapshotGameProxyConfigLocked(*AppSnapshot) {}

func (a *App) snapshotGameProxyStatus(*AppSnapshot) {}

func (a *App) detachGameProxyRunContextLocked() context.CancelFunc { return nil }

func (a *App) shutdownGameProxy() {}

func (a *App) stopGameProxyWatcher() {}

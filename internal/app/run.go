package app

import (
	"context"
	"embed"
	"fmt"
	"log/slog"

	"bork/internal/config"
	"bork/internal/gui"
	"bork/internal/identity"
	"bork/internal/invite"
	"bork/internal/peer"
)

func RunGUI(cfg config.Config, assets embed.FS, logger *slog.Logger) error {
	application := NewApp(cfg, logger)
	return gui.Run(gui.Host{
		Assets:        assets,
		Bindings:      []interface{}{application},
		OnStartup:     application.startup,
		OnBeforeClose: application.beforeClose,
		OnShutdown:    application.shutdown,
	})
}

func RunRelay(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	peerLocalIdentity, err := identity.LoadOrCreate(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("load identity: %w", err)
	}
	identityLease, err := identity.Acquire(peerLocalIdentity)
	if err != nil {
		return fmt.Errorf("activate identity: %w", err)
	}
	defer identityLease.Close()
	encodedInvite, err := cfg.LoadInvite()
	if err != nil {
		return fmt.Errorf("load room invite: %w", err)
	}
	roomInvite, err := invite.Parse(encodedInvite)
	if err != nil {
		return fmt.Errorf("parse room invite: %w", err)
	}
	client := peer.NewClient(peerLocalIdentity, roomInvite, cfg.NetworkOptions(), logger)
	return client.Loop(ctx, nil)
}

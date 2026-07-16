package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"bork/internal/app"
	"bork/internal/config"
)

var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	cfg, err := config.ParseConfig(args, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "bork: %v\n", err)
		return 2
	}
	if cfg.ShowVersion {
		fmt.Printf("bork %s\n", version)
		return 0
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if cfg.Mode == config.ModeRelayPeer {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := app.RunRelay(ctx, cfg, logger); err != nil {
			logger.Error("relay peer stopped", "error", err)
			return 1
		}
		return 0
	}

	if err := app.RunGUI(cfg, assets, logger); err != nil {
		logger.Error("GUI stopped", "error", err)
		return 1
	}
	return 0
}

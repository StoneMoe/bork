package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"bork/internal/app"
	"bork/internal/config"
	"bork/internal/webassets"
)

var version = "dev"

func main() {
	prepareConsole(os.Args[1:])
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

	if err := cfg.EnsureConfigFile(); err != nil {
		fmt.Fprintf(os.Stderr, "bork: %v\n", err)
		return 2
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := app.RunGUI(cfg, webassets.Files, logger); err != nil {
		logger.Error("GUI stopped", "error", err)
		return 1
	}
	return 0
}

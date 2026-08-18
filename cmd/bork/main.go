package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"bork/internal/app"
	"bork/internal/config"
	"bork/internal/webassets"
)

func main() {
	prepareConsole(os.Args[1:])
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("bork", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	showVersion := flags.Bool("version", false, "print version and exit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(os.Stderr, "bork: %v\n", err)
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "bork: unexpected positional arguments: %s\n", strings.Join(flags.Args(), " "))
		return 2
	}
	if *showVersion {
		fmt.Printf("bork %s\n", app.BuildVersion)
		return 0
	}

	cfg, err := config.LoadAppConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bork: %v\n", err)
		return 2
	}
	if err := cfg.EnsureFile(); err != nil {
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

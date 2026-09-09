package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"

	"bork/internal/app"
	"bork/internal/config"
	"bork/internal/logging"
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
	logOutput := io.Writer(os.Stderr)
	logFile, logErr := logging.Open(cfg.LogPath())
	if logErr != nil {
		fmt.Fprintf(os.Stderr, "bork: persistent log unavailable: %v\n", logErr)
	} else {
		defer logFile.Close()
		// Windows GUI binaries can expose an invalid stderr handle. Write the
		// durable file first so a console error cannot suppress persistent logs.
		logOutput = io.MultiWriter(logFile, os.Stderr)
	}
	logger := slog.New(slog.NewTextHandler(logOutput, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("application starting", "version", app.BuildVersion, "os", runtime.GOOS, "arch", runtime.GOARCH, "config", cfg.FilePath, "log", cfg.LogPath())
	if err := app.RunGUI(cfg, webassets.Files, logger); err != nil {
		logger.Error("GUI stopped", "error", err)
		return 1
	}
	return 0
}

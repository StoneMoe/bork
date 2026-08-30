package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

const (
	defaultManifestPath = "internal/gameproxy/netfilter/sdk.lock.json"
	defaultDestination  = "internal/gameproxy/netfilter/sdk"
)

type workspace struct {
	manifestPath string
	destination  string
}

type cli struct {
	stdout    io.Writer
	stderr    io.Writer
	workspace workspace
	rename    renameFunc
}

func main() {
	command := cli{
		stdout: os.Stdout,
		stderr: os.Stderr,
		workspace: workspace{
			manifestPath: defaultManifestPath,
			destination:  defaultDestination,
		},
		rename: os.Rename,
	}
	os.Exit(command.run(os.Args[1:]))
}

func (command cli) run(args []string) int {
	if err := command.execute(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(command.stderr, "netfiltersdk: %v\n", err)
		return 1
	}
	return 0
}

func (command cli) execute(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("expected ingest or verify command")
	}
	lock, err := loadManifest(command.workspace.manifestPath)
	if err != nil {
		return err
	}

	switch args[0] {
	case "ingest":
		flags := flag.NewFlagSet("netfiltersdk ingest", flag.ContinueOnError)
		flags.SetOutput(command.stderr)
		archivePath := flags.String("archive", "", "path to the local pinned SDK ZIP archive")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *archivePath == "" {
			return fmt.Errorf("ingest requires -archive")
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("ingest does not accept positional arguments")
		}
		result, err := (installer{rename: command.rename}).ingest(lock, *archivePath, command.workspace.destination)
		if err != nil {
			return err
		}
		if result.backupCleanupError != nil {
			fmt.Fprintf(command.stderr, "netfiltersdk: warning: remove replaced SDK backup: %v\n", result.backupCleanupError)
		}
		fmt.Fprintf(command.stdout, "ingested NetFilter SDK %s for %s\n", lock.version, lock.platform)
		return nil
	case "verify":
		flags := flag.NewFlagSet("netfiltersdk verify", flag.ContinueOnError)
		flags.SetOutput(command.stderr)
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("verify does not accept positional arguments")
		}
		if err := verifyInstallation(lock, command.workspace.destination); err != nil {
			return err
		}
		fmt.Fprintf(command.stdout, "verified NetFilter SDK %s for %s\n", lock.version, lock.platform)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func loadManifest(manifestPath string) (parsed manifest, returnErr error) {
	file, err := os.Open(manifestPath)
	if err != nil {
		return manifest{}, fmt.Errorf("open manifest %s: %w", manifestPath, err)
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	parsed, err = parseManifest(file)
	if err != nil {
		return manifest{}, fmt.Errorf("parse manifest %s: %w", manifestPath, err)
	}
	return parsed, nil
}

package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"bork/internal/invite"
	"bork/internal/networking"
	"bork/internal/networking/endpoint"
)

type Config struct {
	Invite      string   `json:"-"`
	InviteFile  string   `json:"-"`
	DataDir     string   `json:"-"`
	UDPListen   string   `json:"-"`
	STUNServers []string `json:"-"`
	TrackerURLs []string `json:"-"`
	PortMapping bool     `json:"-"`
	ConfigFile  string   `json:"-"`
	ShowVersion bool     `json:"-"`
}

func ParseConfig(args []string, output io.Writer) (Config, error) {
	var cfg Config

	flags := flag.NewFlagSet("bork", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&cfg.Invite, "join", "", "join a room using an encoded invite")
	flags.StringVar(&cfg.InviteFile, "join-file", "", "read the room invite from a file")
	flags.StringVar(&cfg.DataDir, "data-dir", "", "directory for the persistent user identity")
	flags.BoolVar(&cfg.ShowVersion, "version", false, "print version and exit")
	flags.Usage = func() {
		fmt.Fprintln(output, "Usage: bork [options]")
		flags.PrintDefaults()
	}

	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	if flags.NArg() != 0 {
		return Config{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	if cfg.ShowVersion {
		return cfg, nil
	}
	if strings.TrimSpace(cfg.Invite) != "" && strings.TrimSpace(cfg.InviteFile) != "" {
		return Config{}, errors.New("--join and --join-file cannot be used together")
	}
	configFile, err := behaviorConfigPath()
	if err != nil {
		return Config{}, err
	}
	behavior, err := loadBehaviorConfig(configFile)
	if err != nil {
		return Config{}, err
	}
	cfg.ConfigFile = configFile
	cfg.UDPListen = behavior.UDPListen
	cfg.STUNServers = behavior.STUNServers
	cfg.TrackerURLs = behavior.TrackerURLs
	cfg.PortMapping = behavior.PortMapping
	return cfg, nil
}

func (c Config) NetworkOptions() networking.Options {
	options := endpoint.DefaultOptions()
	options.ListenAddress = c.UDPListen
	options.STUNServers = append([]string{}, c.STUNServers...)
	return networking.Options{
		Endpoint:          options,
		TrackerURLs:       append([]string{}, c.TrackerURLs...),
		EnablePortMapping: c.PortMapping,
	}
}

func (c Config) LoadInvite() (string, error) {
	if strings.TrimSpace(c.Invite) != "" {
		return strings.TrimSpace(c.Invite), nil
	}
	if strings.TrimSpace(c.InviteFile) == "" {
		return "", nil
	}
	info, err := os.Lstat(c.InviteFile)
	if err != nil {
		return "", fmt.Errorf("inspect invite file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("invite file must be a regular file")
	}
	file, err := os.Open(c.InviteFile)
	if err != nil {
		return "", fmt.Errorf("read invite file: %w", err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect opened invite file: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return "", errors.New("invite file changed while opening")
	}
	contents, err := io.ReadAll(io.LimitReader(file, invite.MaxEncodedSize+1))
	if err != nil {
		return "", fmt.Errorf("read invite file: %w", err)
	}
	if len(contents) > invite.MaxEncodedSize {
		return "", errors.New("invite file is too large")
	}
	encoded := strings.TrimSpace(string(contents))
	if encoded == "" {
		return "", errors.New("invite file is empty")
	}
	return encoded, nil
}

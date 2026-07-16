package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"bork/internal/invite"
	"bork/internal/networking/endpoint"
)

type Mode string

const (
	ModeGUI       Mode = "gui-peer"
	ModeRelayPeer Mode = "relay-peer"
)

type Config struct {
	Mode        Mode     `json:"mode"`
	Invite      string   `json:"-"`
	InviteFile  string   `json:"-"`
	DataDir     string   `json:"-"`
	UDPListen   string   `json:"-"`
	STUNServers []string `json:"-"`
	ShowVersion bool     `json:"-"`
}

func ParseConfig(args []string, output io.Writer) (Config, error) {
	networkDefaults := endpoint.DefaultOptions()
	cfg := Config{Mode: ModeGUI, UDPListen: networkDefaults.ListenAddress}
	stunServers := strings.Join(networkDefaults.STUNServers, ",")

	flags := flag.NewFlagSet("bork", flag.ContinueOnError)
	flags.SetOutput(output)
	relayPeer := flags.Bool("relay-peer", false, "run without a GUI as a room-scoped relay candidate")
	flags.StringVar(&cfg.Invite, "join", "", "join a room using an encoded invite")
	flags.StringVar(&cfg.InviteFile, "join-file", "", "read the room invite from a file")
	flags.StringVar(&cfg.DataDir, "data-dir", "", "directory for the persistent user identity")
	flags.StringVar(&cfg.UDPListen, "udp-listen", cfg.UDPListen, "UDP listen address shared by discovery, control, and media")
	flags.StringVar(&stunServers, "stun-servers", stunServers, "comma-separated STUN servers; empty disables STUN")
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
	if err := validateUDPListen(cfg.UDPListen); err != nil {
		return Config{}, err
	}
	parsedSTUN, err := parseSTUNServers(stunServers)
	if err != nil {
		return Config{}, err
	}
	cfg.STUNServers = parsedSTUN
	if strings.TrimSpace(cfg.Invite) != "" && strings.TrimSpace(cfg.InviteFile) != "" {
		return Config{}, errors.New("--join and --join-file cannot be used together")
	}
	if *relayPeer {
		cfg.Mode = ModeRelayPeer
		if strings.TrimSpace(cfg.Invite) == "" && strings.TrimSpace(cfg.InviteFile) == "" {
			return Config{}, errors.New("--relay-peer requires --join or --join-file")
		}
	}
	return cfg, nil
}

func (c Config) NetworkOptions() endpoint.Options {
	options := endpoint.DefaultOptions()
	options.ListenAddress = c.UDPListen
	options.STUNServers = append([]string{}, c.STUNServers...)
	return options
}

func validateUDPListen(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid --udp-listen address %q: %w", address, err)
	}
	if host != "localhost" && net.ParseIP(host) == nil {
		return fmt.Errorf("invalid --udp-listen address %q: host must be an IP literal or localhost", address)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return fmt.Errorf("invalid --udp-listen address %q: port must be between 0 and 65535", address)
	}
	return nil
}

func parseSTUNServers(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return []string{}, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) > 8 {
		return nil, errors.New("--stun-servers supports at most 8 servers")
	}
	servers := make([]string, 0, len(parts))
	seen := make(map[string]struct{})
	for _, part := range parts {
		server := strings.TrimSpace(part)
		host, port, err := net.SplitHostPort(server)
		if err != nil || host == "" {
			return nil, fmt.Errorf("invalid STUN server %q: expected host:port", server)
		}
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return nil, fmt.Errorf("invalid STUN server %q: port must be between 1 and 65535", server)
		}
		if _, exists := seen[server]; exists {
			continue
		}
		seen[server] = struct{}{}
		servers = append(servers, server)
	}
	return servers, nil
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

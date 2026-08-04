package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"bork/internal/networking/discovery/tracker"
	"bork/internal/networking/endpoint"

	"gopkg.in/yaml.v3"
)

const (
	behaviorConfigFilename = "config.yml"
	maxBehaviorConfigSize  = 64 * 1024
)

var (
	userConfigDir      = os.UserConfigDir
	linkConfigFile     = os.Link
	defaultSTUNServers = []string{
		"stun.cloudflare.com:3478",
		"stun.miwifi.com:3478",
	}
	defaultTrackerURLs = []string{
		"https://tracker.zhuqiy.com/announce",
		"http://tracker.renfei.net:8080/announce",
		"http://tracker.mywaifu.best:6969/announce",
	}
)

type behaviorConfig struct {
	UDPListen   string   `yaml:"udp_listen"`
	STUNServers []string `yaml:"stun_servers"`
	TrackerURLs []string `yaml:"tracker_urls"`
	PortMapping bool     `yaml:"port_mapping"`
}

type behaviorYAML struct {
	Network *networkYAML `yaml:"network,omitempty"`
}

type networkYAML struct {
	UDPListen   *string   `yaml:"udp_listen,omitempty"`
	STUNServers *[]string `yaml:"stun_servers,omitempty"`
	TrackerURLs *[]string `yaml:"tracker_urls,omitempty"`
	PortMapping *bool     `yaml:"port_mapping,omitempty"`
}

type persistedBehaviorYAML struct {
	Network behaviorConfig `yaml:"network"`
}

func defaultBehaviorConfig() behaviorConfig {
	return behaviorConfig{
		UDPListen: endpoint.DefaultOptions().ListenAddress, STUNServers: append([]string{}, defaultSTUNServers...),
		TrackerURLs: append([]string{}, defaultTrackerURLs...), PortMapping: true,
	}
}

func behaviorConfigPath() (string, error) {
	base, err := userConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config directory: %w", err)
	}
	path := filepath.Join(base, "bork", behaviorConfigFilename)
	resolved, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve config path: %w", err)
	}
	return resolved, nil
}

func loadBehaviorConfig(path string) (behaviorConfig, error) {
	config := defaultBehaviorConfig()
	contents, exists, err := readBehaviorConfig(path)
	if err != nil {
		return behaviorConfig{}, fmt.Errorf("load client config %q: %w", path, err)
	}
	if !exists || len(bytes.TrimSpace(contents)) == 0 {
		return config, nil
	}
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)
	var encoded behaviorYAML
	if err := decoder.Decode(&encoded); err != nil {
		return behaviorConfig{}, fmt.Errorf("parse client config %q: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return behaviorConfig{}, fmt.Errorf("parse client config %q: multiple YAML documents are not allowed", path)
		}
		return behaviorConfig{}, fmt.Errorf("parse client config %q: %w", path, err)
	}
	if encoded.Network != nil {
		if encoded.Network.UDPListen != nil {
			config.UDPListen = strings.TrimSpace(*encoded.Network.UDPListen)
		}
		if encoded.Network.STUNServers != nil {
			config.STUNServers = append([]string{}, (*encoded.Network.STUNServers)...)
		}
		if encoded.Network.TrackerURLs != nil {
			config.TrackerURLs = append([]string{}, (*encoded.Network.TrackerURLs)...)
		}
		if encoded.Network.PortMapping != nil {
			config.PortMapping = *encoded.Network.PortMapping
		}
	}
	if err := validateUDPListen(config.UDPListen); err != nil {
		return behaviorConfig{}, fmt.Errorf("validate client config %q: %w", path, err)
	}
	config.STUNServers, err = validateSTUNServers(config.STUNServers)
	if err != nil {
		return behaviorConfig{}, fmt.Errorf("validate client config %q: %w", path, err)
	}
	config.TrackerURLs, err = validateTrackerURLs(config.TrackerURLs)
	if err != nil {
		return behaviorConfig{}, fmt.Errorf("validate client config %q: %w", path, err)
	}
	return config, nil
}

func readBehaviorConfig(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxBehaviorConfigSize {
		return nil, false, errors.New("config must be a regular file no larger than 64 KiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, false, errors.New("config changed while opening")
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxBehaviorConfigSize+1))
	if err != nil {
		return nil, false, err
	}
	if len(contents) > maxBehaviorConfigSize {
		return nil, false, errors.New("config is larger than 64 KiB")
	}
	return contents, true, nil
}

func (c Config) EnsureConfigFile() error {
	if c.ConfigFile == "" {
		return errors.New("client config path is empty")
	}
	if info, err := os.Lstat(c.ConfigFile); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("client config is not a regular file")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect client config: %w", err)
	}
	parent := filepath.Dir(c.ConfigFile)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create client config directory: %w", err)
	}
	contents, err := yaml.Marshal(persistedBehaviorYAML{Network: behaviorConfig{
		UDPListen: c.UDPListen, STUNServers: c.STUNServers, TrackerURLs: c.TrackerURLs, PortMapping: c.PortMapping,
	}})
	if err != nil {
		return fmt.Errorf("encode client config: %w", err)
	}
	temporary, err := os.CreateTemp(parent, ".config-*")
	if err != nil {
		return fmt.Errorf("create temporary client config: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure temporary client config: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary client config: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary client config: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary client config: %w", err)
	}
	if err := publishConfigNoReplace(temporaryPath, c.ConfigFile); err != nil {
		return fmt.Errorf("publish client config: %w", err)
	}
	return nil
}

func publishConfigNoReplace(temporaryPath, targetPath string) error {
	linkErr := linkConfigFile(temporaryPath, targetPath)
	if linkErr == nil {
		return nil
	}
	if info, err := os.Lstat(targetPath); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("client config appeared as a non-regular file")
		}
		_, loadErr := loadBehaviorConfig(targetPath)
		return loadErr
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return linkErr
}

func validateUDPListen(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid UDP listen address %q: %w", address, err)
	}
	if host != "localhost" && net.ParseIP(host) == nil {
		return fmt.Errorf("invalid UDP listen address %q: host must be an IP literal or localhost", address)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return fmt.Errorf("invalid UDP listen address %q: port must be between 0 and 65535", address)
	}
	return nil
}

func validateSTUNServers(raw []string) ([]string, error) {
	if len(raw) > 8 {
		return nil, errors.New("stun_servers supports at most 8 servers")
	}
	servers := make([]string, 0, len(raw))
	seen := make(map[string]struct{})
	for _, part := range raw {
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

func validateTrackerURLs(raw []string) ([]string, error) {
	if len(raw) > 8 {
		return nil, errors.New("tracker_urls supports at most 8 providers")
	}
	providers := make([]string, 0, len(raw))
	seen := make(map[string]struct{})
	for _, part := range raw {
		provider := strings.TrimSpace(part)
		if err := tracker.ValidateProviderURL(provider); err != nil {
			return nil, fmt.Errorf("invalid tracker URL %q: %w", provider, err)
		}
		if _, exists := seen[provider]; exists {
			continue
		}
		seen[provider] = struct{}{}
		providers = append(providers, provider)
	}
	return providers, nil
}

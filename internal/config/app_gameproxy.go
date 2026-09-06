//go:build game_proxy

package config

import "gopkg.in/yaml.v3"

type AppConfig struct {
	FilePath  string          `yaml:"-"`
	Network   NetworkConfig   `yaml:"network"`
	GameProxy GameProxyConfig `yaml:"game_proxy"`
}

func decodeAppConfig(contents []byte) (AppConfig, error) {
	config := AppConfig{
		Network:   defaultNetworkConfig(),
		GameProxy: defaultGameProxyConfig(),
	}
	if err := decodeConfigYAML(contents, &config); err != nil {
		return AppConfig{}, err
	}
	var err error
	config.GameProxy.Node, err = validateStoredGameProxyNode(config.GameProxy.Node)
	if err != nil {
		return AppConfig{}, err
	}
	config.GameProxy.Directories = normalizeConfigDirectories(config.GameProxy.Directories)
	return config, nil
}

func encodeAppConfig(config AppConfig, _ []byte) ([]byte, error) {
	return yaml.Marshal(config)
}

//go:build !game_proxy

package config

import (
	"errors"

	"gopkg.in/yaml.v3"
)

type AppConfig struct {
	FilePath string        `yaml:"-"`
	Network  NetworkConfig `yaml:"network"`
}

// The disabled feature is disk-only data, never part of the application config.
type configDocument struct {
	Network   NetworkConfig `yaml:"network"`
	GameProxy yaml.Node     `yaml:"game_proxy,omitempty"`
}

func decodeConfigDocument(contents []byte) (configDocument, error) {
	document := configDocument{Network: defaultNetworkConfig()}
	if err := decodeConfigYAML(contents, &document); err != nil {
		return configDocument{}, err
	}
	if document.GameProxy.Kind != 0 {
		// Check duplicate keys, cycles and excessive alias expansion before copying.
		var ignored any
		if err := document.GameProxy.Decode(&ignored); err != nil {
			return configDocument{}, err
		}
		// Anchors may belong to the old network section, which Save replaces.
		expanded, err := expandConfigAliases(document.GameProxy, make(map[*yaml.Node]bool))
		if err != nil {
			return configDocument{}, err
		}
		document.GameProxy = expanded
	}
	return document, nil
}

func decodeAppConfig(contents []byte) (AppConfig, error) {
	document, err := decodeConfigDocument(contents)
	return AppConfig{Network: document.Network}, err
}

func encodeAppConfig(config AppConfig, existing []byte) ([]byte, error) {
	document, err := decodeConfigDocument(existing)
	if err != nil {
		return nil, err
	}
	document.Network = config.Network
	return yaml.Marshal(document)
}

func expandConfigAliases(node yaml.Node, aliases map[*yaml.Node]bool) (yaml.Node, error) {
	if node.Kind == yaml.AliasNode {
		// YAML merge overrides can hide a cycle from Decode, but not this copy.
		if aliases[node.Alias] {
			return yaml.Node{}, errors.New("config contains a cyclic YAML alias")
		}
		aliases[node.Alias] = true
		defer delete(aliases, node.Alias)
		node = *node.Alias
	}
	node.Anchor = ""
	children := make([]*yaml.Node, len(node.Content))
	for i, child := range node.Content {
		copy, err := expandConfigAliases(*child, aliases)
		if err != nil {
			return yaml.Node{}, err
		}
		children[i] = &copy
	}
	node.Content = children
	return node, nil
}

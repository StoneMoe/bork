package config

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"unicode/utf8"
)

const gameProxyNodeFieldCount = 6

type GameProxyConfig struct {
	Directory string              `yaml:"directory"`
	Node      GameProxyNodeConfig `yaml:"node"`
}

type GameProxyNodeConfig struct {
	Server   string `yaml:"server" json:"server"`
	Port     int    `yaml:"port" json:"port"`
	Username string `yaml:"username" json:"username"`
	Password string `yaml:"password" json:"password"`
	MTU      int    `yaml:"mtu" json:"mtu"`
	DNS      string `yaml:"dns" json:"dns"`
}

func defaultGameProxyConfig() GameProxyConfig {
	return GameProxyConfig{
		Node: GameProxyNodeConfig{
			Port: 4567,
			MTU:  1400,
			DNS:  "1.1.1.1",
		},
	}
}

func ValidateGameProxyNode(node GameProxyNodeConfig) (GameProxyNodeConfig, error) {
	node.Server = strings.TrimSpace(node.Server)
	if node.Server == "" {
		return GameProxyNodeConfig{}, errors.New("game proxy server must not be empty")
	}
	node.Username = strings.TrimSpace(node.Username)
	if node.Username == "" {
		return GameProxyNodeConfig{}, errors.New("game proxy username must not be empty")
	}
	if !utf8.ValidString(node.Username) || len(node.Username) > 253 {
		return GameProxyNodeConfig{}, errors.New("game proxy username must be valid UTF-8 and at most 253 bytes")
	}
	if node.Password == "" {
		return GameProxyNodeConfig{}, errors.New("game proxy password must not be empty")
	}
	if !utf8.ValidString(node.Password) || len(node.Password) > 16 {
		return GameProxyNodeConfig{}, errors.New("game proxy password must be valid UTF-8 and at most 16 bytes")
	}
	if err := validateGameProxyNetworkValues(node); err != nil {
		return GameProxyNodeConfig{}, err
	}
	return node, nil
}

func validateStoredGameProxyNode(node GameProxyNodeConfig) (GameProxyNodeConfig, error) {
	if node.Server == "" && node.Username == "" && node.Password == "" {
		if err := validateGameProxyNetworkValues(node); err != nil {
			return GameProxyNodeConfig{}, err
		}
		return node, nil
	}
	return ValidateGameProxyNode(node)
}

func validateGameProxyNetworkValues(node GameProxyNodeConfig) error {
	if node.Port < 1 || node.Port > 65535 {
		return errors.New("game proxy port must be between 1 and 65535")
	}
	if node.MTU < 46 || node.MTU > 1600 {
		return errors.New("game proxy MTU must be between 46 and 1600")
	}
	dns, err := netip.ParseAddr(node.DNS)
	if err != nil || !dns.Is4() || !dns.IsGlobalUnicast() {
		return errors.New("game proxy DNS must be a unicast IPv4 address")
	}
	return nil
}

func (config GameProxyConfig) ExportNodeBase64() (string, error) {
	node, err := ValidateGameProxyNode(config.Node)
	if err != nil {
		return "", fmt.Errorf("validate game proxy node: %w", err)
	}
	contents, err := json.Marshal(node)
	if err != nil {
		return "", fmt.Errorf("encode game proxy node: %w", err)
	}
	return base64.StdEncoding.EncodeToString(contents), nil
}

func (config *GameProxyConfig) ImportNodeBase64(encoded string) error {
	contents, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("decode game proxy node Base64: %w", err)
	}
	node, err := decodeGameProxyNodeJSON(contents)
	if err != nil {
		return err
	}
	config.Node = node
	return nil
}

func decodeGameProxyNodeJSON(contents []byte) (GameProxyNodeConfig, error) {
	if !utf8.Valid(contents) {
		return GameProxyNodeConfig{}, errors.New("decode game proxy node JSON: input must be valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	opening, err := decoder.Token()
	if err != nil {
		return GameProxyNodeConfig{}, fmt.Errorf("decode game proxy node JSON: %w", err)
	}
	if opening != json.Delim('{') {
		return GameProxyNodeConfig{}, errors.New("decode game proxy node JSON: expected an object")
	}

	var node GameProxyNodeConfig
	seen := make(map[string]struct{}, gameProxyNodeFieldCount)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return GameProxyNodeConfig{}, fmt.Errorf("decode game proxy node JSON key: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return GameProxyNodeConfig{}, errors.New("decode game proxy node JSON: object key must be a string")
		}
		if _, exists := seen[key]; exists {
			return GameProxyNodeConfig{}, fmt.Errorf("decode game proxy node JSON: duplicate key %q", key)
		}
		seen[key] = struct{}{}
		if err := decodeGameProxyNodeField(decoder, key, &node); err != nil {
			return GameProxyNodeConfig{}, err
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return GameProxyNodeConfig{}, fmt.Errorf("decode game proxy node JSON: %w", err)
	}
	if closing != json.Delim('}') {
		return GameProxyNodeConfig{}, errors.New("decode game proxy node JSON: expected object end")
	}
	if len(seen) != gameProxyNodeFieldCount {
		return GameProxyNodeConfig{}, errors.New("decode game proxy node JSON: all six node keys are required")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return GameProxyNodeConfig{}, errors.New("decode game proxy node JSON: trailing value is not allowed")
		}
		return GameProxyNodeConfig{}, fmt.Errorf("decode game proxy node JSON trailing value: %w", err)
	}
	validated, err := ValidateGameProxyNode(node)
	if err != nil {
		return GameProxyNodeConfig{}, fmt.Errorf("validate imported game proxy node: %w", err)
	}
	return validated, nil
}

func decodeGameProxyNodeField(decoder *json.Decoder, key string, node *GameProxyNodeConfig) error {
	var err error
	switch key {
	case "server":
		err = decoder.Decode(&node.Server)
	case "port":
		err = decoder.Decode(&node.Port)
	case "username":
		err = decoder.Decode(&node.Username)
	case "password":
		err = decoder.Decode(&node.Password)
	case "mtu":
		err = decoder.Decode(&node.MTU)
	case "dns":
		err = decoder.Decode(&node.DNS)
	default:
		return fmt.Errorf("decode game proxy node JSON: unknown key %q", key)
	}
	if err != nil {
		return fmt.Errorf("decode game proxy node JSON key %q: %w", key, err)
	}
	return nil
}

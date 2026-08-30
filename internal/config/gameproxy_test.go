package config

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGameProxyDefaultsWhenConfigOmitsSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")

	config, err := loadAppConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}

	want := GameProxyConfig{
		Node: GameProxyNodeConfig{
			Port: 4567,
			MTU:  1400,
			DNS:  "1.1.1.1",
		},
	}
	if !reflect.DeepEqual(config.GameProxy, want) {
		t.Fatal("GameProxy defaults did not match")
	}
}

func TestLoadAppConfigFileLoadsLocalDirectoryAndNormalizesNode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	contents := []byte("game_proxy:\n  directory: /local/games\n  node:\n    server: ' proxy.example.com '\n    port: 4568\n    username: ' player '\n    password: ' secret '\n    mtu: 1280\n    dns: 8.8.8.8\n")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}

	config, err := loadAppConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}

	want := GameProxyConfig{
		Directory: "/local/games",
		Node: GameProxyNodeConfig{
			Server:   "proxy.example.com",
			Port:     4568,
			Username: "player",
			Password: " secret ",
			MTU:      1280,
			DNS:      "8.8.8.8",
		},
	}
	if !reflect.DeepEqual(config.GameProxy, want) {
		t.Fatal("loaded GameProxy config did not match")
	}
}

func TestValidateGameProxyNodeNormalizesNamesButPreservesPassword(t *testing.T) {
	node := validGameProxyNode()
	node.Server = "  proxy.example.com  "
	node.Username = "  player  "
	node.Password = "  secret  "

	validated, err := ValidateGameProxyNode(node)
	if err != nil {
		t.Fatal(err)
	}

	if validated.Server != "proxy.example.com" || validated.Username != "player" || validated.Password != "  secret  " {
		t.Fatal("validated node did not preserve the expected values")
	}
}

func TestValidateGameProxyNodeRejectsInvalidActivationValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*GameProxyNodeConfig)
	}{
		{"blank server", func(node *GameProxyNodeConfig) { node.Server = " \t " }},
		{"blank username", func(node *GameProxyNodeConfig) { node.Username = " \t " }},
		{"empty password", func(node *GameProxyNodeConfig) { node.Password = "" }},
		{"port below range", func(node *GameProxyNodeConfig) { node.Port = 0 }},
		{"port above range", func(node *GameProxyNodeConfig) { node.Port = 65536 }},
		{"mtu below range", func(node *GameProxyNodeConfig) { node.MTU = 45 }},
		{"mtu above range", func(node *GameProxyNodeConfig) { node.MTU = 1601 }},
		{"ipv6 dns", func(node *GameProxyNodeConfig) { node.DNS = "2606:4700:4700::1111" }},
		{"unspecified dns", func(node *GameProxyNodeConfig) { node.DNS = "0.0.0.0" }},
		{"multicast dns", func(node *GameProxyNodeConfig) { node.DNS = "224.0.0.1" }},
		{"long username", func(node *GameProxyNodeConfig) { node.Username = strings.Repeat("u", 254) }},
		{"long password", func(node *GameProxyNodeConfig) { node.Password = strings.Repeat("p", 17) }},
		{"invalid username utf8", func(node *GameProxyNodeConfig) { node.Username = string([]byte{0xff}) }},
		{"invalid password utf8", func(node *GameProxyNodeConfig) { node.Password = string([]byte{0xff}) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			node := validGameProxyNode()
			test.mutate(&node)

			if _, err := ValidateGameProxyNode(node); err == nil {
				t.Fatal("ValidateGameProxyNode succeeded")
			}
		})
	}
}

func TestValidateGameProxyNodeUsesUTF8ByteLimits(t *testing.T) {
	node := validGameProxyNode()
	node.Username = strings.Repeat("界", 84) + "a"
	node.Password = strings.Repeat("界", 5) + "a"

	validated, err := ValidateGameProxyNode(node)
	if err != nil {
		t.Fatal(err)
	}
	if validated.Username != node.Username || validated.Password != node.Password {
		t.Fatal("validated node did not preserve UTF-8 boundary values")
	}
}

func TestGameProxyExportNodeBase64ContainsExactlySixTypedKeys(t *testing.T) {
	config := GameProxyConfig{Directory: "/must/stay/local", Node: validGameProxyNode()}

	encoded, err := config.ExportNodeBase64()
	if err != nil {
		t.Fatal(err)
	}
	contents, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(contents, &fields); err != nil {
		t.Fatal(err)
	}

	wantKeys := []string{"server", "port", "username", "password", "mtu", "dns"}
	if len(fields) != len(wantKeys) {
		t.Fatal("exported JSON did not contain exactly six keys")
	}
	for _, key := range wantKeys {
		if _, exists := fields[key]; !exists {
			t.Fatalf("missing key %q", key)
		}
	}
	if string(fields["port"]) != "4567" || string(fields["mtu"]) != "1400" {
		t.Fatalf("numeric fields = port %s, mtu %s", fields["port"], fields["mtu"])
	}
}

func TestGameProxyImportNodeBase64ReplacesOnlyNode(t *testing.T) {
	config := GameProxyConfig{Directory: "/local/games", Node: validGameProxyNode()}
	encoded := encodeNodeJSON(`{"server":"new.example.com","port":1234,"username":"new-user","password":" new-pass ","mtu":1280,"dns":"8.8.8.8"}`)

	if err := config.ImportNodeBase64(encoded); err != nil {
		t.Fatal(err)
	}

	if config.Directory != "/local/games" {
		t.Fatalf("Directory = %q", config.Directory)
	}
	if config.Node.Server != "new.example.com" || config.Node.Password != " new-pass " || config.Node.Port != 1234 {
		t.Fatal("imported node did not match")
	}
}

func TestGameProxyImportNodeBase64RejectsStrictJSONWithoutMutation(t *testing.T) {
	valid := `{"server":"proxy.example.com","port":4567,"username":"player","password":"secret","mtu":1400,"dns":"1.1.1.1"}`
	tests := []struct {
		name    string
		encoded string
	}{
		{"invalid base64", "not@base64"},
		{"duplicate key", encodeNodeJSON(`{"server":"a","server":"b","port":4567,"username":"player","password":"secret","mtu":1400,"dns":"1.1.1.1"}`)},
		{"unknown key", encodeNodeJSON(`{"server":"a","port":4567,"username":"player","password":"secret","mtu":1400,"dns":"1.1.1.1","directory":"remote"}`)},
		{"missing key", encodeNodeJSON(`{"server":"a","port":4567,"username":"player","password":"secret","mtu":1400}`)},
		{"wrong type", encodeNodeJSON(`{"server":"a","port":"4567","username":"player","password":"secret","mtu":1400,"dns":"1.1.1.1"}`)},
		{"trailing value", encodeNodeJSON(valid + `{}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := GameProxyConfig{Directory: "/local/games", Node: validGameProxyNode()}
			before := config

			if err := config.ImportNodeBase64(test.encoded); err == nil {
				t.Fatal("ImportNodeBase64 succeeded")
			}
			if !reflect.DeepEqual(config, before) {
				t.Fatal("config changed after rejected import")
			}
		})
	}
}

func validGameProxyNode() GameProxyNodeConfig {
	return GameProxyNodeConfig{
		Server:   "proxy.example.com",
		Port:     4567,
		Username: "player",
		Password: "secret",
		MTU:      1400,
		DNS:      "1.1.1.1",
	}
}

func encodeNodeJSON(contents string) string {
	return base64.StdEncoding.EncodeToString([]byte(contents))
}

//go:build windows && amd64 && game_proxy

package netfilter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLicense_preserves_original_RTF_through_JSON(t *testing.T) {
	license, err := License()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(license, `{\rtf1`) || license != embeddedNetFilterLicense {
		t.Fatal("license is not the original embedded RTF")
	}
	encoded, err := json.Marshal(license)
	if err != nil {
		t.Fatal(err)
	}
	var decoded string
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != embeddedNetFilterLicense {
		t.Fatal("JSON transport changed the license bytes")
	}
}

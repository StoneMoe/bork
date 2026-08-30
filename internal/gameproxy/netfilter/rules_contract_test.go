package netfilter

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestExactRules_rejects_wildcard_case_duplicate_and_long_UTF16_path(t *testing.T) {
	tests := []struct {
		name  string
		paths []string
		want  error
	}{
		{name: "wildcard", paths: []string{`C:\Games\*.exe`}, want: ErrInvalidExecutablePath},
		{name: "case duplicate", paths: []string{`C:\Games\game.exe`, `c:\games\GAME.exe`}, want: ErrDuplicateExecutablePath},
		{name: "more than 259 UTF-16 units", paths: []string{`C:\` + strings.Repeat("a", 253) + ".exe"}, want: ErrInvalidExecutablePath},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// When
			_, err := exactRules(test.paths)

			// Then
			if !errors.Is(err, test.want) {
				t.Fatalf("exactRules() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestExactRules_accepts_path_with_exactly_259_UTF16_units(t *testing.T) {
	// Given
	path := `C:\` + strings.Repeat("a", 252) + ".exe"
	if units := len(utf16.Encode([]rune(path))); units != 259 {
		t.Fatalf("test path UTF-16 units = %d, want 259", units)
	}

	// When
	rules, err := exactRules([]string{path})

	// Then
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatalf("rule count = %d, want 2", len(rules))
	}
}

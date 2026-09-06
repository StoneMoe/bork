//go:build windows && game_proxy

package gameproxy

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestScanExecutableRulesDoesNotInspectNonExecutables(t *testing.T) {
	root := t.TempDir()
	executable := writeTestFile(t, filepath.Join(root, "00-game.exe"))
	ignored := writeTestFile(t, filepath.Join(root, "01-ignored.bin"))
	canonicalize := func(path string) (string, error) {
		if path == executable {
			// WalkDir has already read both entries. A later attribute lookup
			// for the irrelevant file would now fail instead of being skipped.
			if err := os.Remove(ignored); err != nil {
				return "", err
			}
		}
		return canonicalPath(path)
	}

	rules, err := scanExecutableRulesFromRoots(t.Context(), []string{root}, canonicalize)
	if err != nil {
		t.Fatal(err)
	}
	want, err := canonicalPath(executable)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rules.Paths(), []string{want}) {
		t.Fatalf("Paths() = %q, want %q", rules.Paths(), []string{want})
	}
}

func TestNormalizeWindowsPathRemovesDevicePrefixesAndNormalizesCaseAndSeparators(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "drive path", path: `\\?\C:\Games/Mixed\Game.EXE`, want: `c:\games\mixed\game.exe`},
		{name: "UNC path", path: `\\?\UNC\Server\Share/Game.EXE`, want: `\\server\share\game.exe`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := normalizeWindowsPath(test.path); got != test.want {
				t.Fatalf("normalizeWindowsPath(%q) = %q, want %q", test.path, got, test.want)
			}
		})
	}
}

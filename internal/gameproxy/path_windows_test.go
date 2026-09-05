//go:build windows

package gameproxy

import "testing"

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

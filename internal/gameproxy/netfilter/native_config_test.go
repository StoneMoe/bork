package netfilter

import (
	"errors"
	"testing"
)

func TestNewNativeConfig_accepts_absolute_nfapi_DLL_and_driver_name(t *testing.T) {
	// Given
	dllPath := `C:\Program Files\NetFilter\NFAPI.DLL`
	driverName := "netfilter2"

	// When
	config, err := newNativeConfig(dllPath, driverName)

	// Then
	if err != nil {
		t.Fatal(err)
	}
	if config.dllPath != dllPath {
		t.Fatalf("DLL path = %q, want %q", config.dllPath, dllPath)
	}
	if config.normalizedDLLPath != `c:\program files\netfilter\nfapi.dll` {
		t.Fatalf("normalized DLL path = %q", config.normalizedDLLPath)
	}
	if config.driverName != driverName {
		t.Fatalf("driver name = %q, want %q", config.driverName, driverName)
	}
}

func TestNewNativeConfig_rejects_invalid_DLL_path(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "empty", path: ""},
		{name: "relative", path: `nfapi.dll`},
		{name: "wrong basename", path: `C:\sdk\other.dll`},
		{name: "embedded NUL", path: "C:\\sdk\\nfapi.dll\x00"},
		{name: "extended namespace", path: `\\?\C:\sdk\nfapi.dll`},
		{name: "device namespace", path: `\\.\C:\sdk\nfapi.dll`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// When
			_, err := newNativeConfig(test.path, "netfilter2")

			// Then
			if !errors.Is(err, ErrInvalidNativeDLLPath) {
				t.Fatalf("newNativeConfig() error = %v, want ErrInvalidNativeDLLPath", err)
			}
		})
	}
}

func TestNewNativeConfig_rejects_invalid_driver_name(t *testing.T) {
	tests := []string{"", "net/filter", `net\\filter`, "netfilter2.sys", "NETFILTER2.SYS", "net\x00filter"}
	for _, driverName := range tests {
		t.Run(driverName, func(t *testing.T) {
			// When
			_, err := newNativeConfig(`\\server\share\nfapi.dll`, driverName)

			// Then
			if !errors.Is(err, ErrInvalidNativeDriverName) {
				t.Fatalf("newNativeConfig() error = %v, want ErrInvalidNativeDriverName", err)
			}
		})
	}
}

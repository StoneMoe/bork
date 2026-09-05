package netfilter

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

type nativeConfig struct {
	dllPath           string
	normalizedDLLPath string
	driverName        string
}

func newNativeConfig(dllPath, driverName string) (nativeConfig, error) {
	if !validNativeDLLPath(dllPath) {
		return nativeConfig{}, fmt.Errorf("DLL path %q: %w", dllPath, ErrInvalidNativeDLLPath)
	}
	if !validNativeDriverName(driverName) {
		return nativeConfig{}, fmt.Errorf("driver name %q: %w", driverName, ErrInvalidNativeDriverName)
	}
	return nativeConfig{
		dllPath:           dllPath,
		normalizedDLLPath: strings.ToLower(dllPath),
		driverName:        driverName,
	}, nil
}

func validNativeDLLPath(path string) bool {
	if !utf8.ValidString(path) || strings.IndexByte(path, 0) >= 0 || !isAbsoluteWindowsPath(path) {
		return false
	}
	separator := strings.LastIndexByte(path, '\\')
	return separator >= 0 && strings.EqualFold(path[separator+1:], "nfapi.dll")
}

func validNativeDriverName(name string) bool {
	return name != "" && utf8.ValidString(name) && strings.IndexByte(name, 0) < 0 &&
		!strings.ContainsAny(name, `/\\`) && !strings.HasSuffix(strings.ToLower(name), ".sys")
}

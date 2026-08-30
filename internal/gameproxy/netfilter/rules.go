package netfilter

import (
	"slices"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

func exactRules(executablePaths []string) ([]nativeRule, error) {
	if len(executablePaths) == 0 {
		return nil, ErrEmptyExecutablePaths
	}
	paths := slices.Clone(executablePaths)
	seen := make(map[string]struct{}, len(paths))
	for index, path := range paths {
		if path == "" || !utf8.ValidString(path) || strings.IndexByte(path, 0) >= 0 ||
			strings.Contains(path, "*") || !isAbsoluteWindowsPath(path) ||
			len(utf16.Encode([]rune(path))) > 259 {
			return nil, &ExecutablePathError{Index: index, Path: path, Cause: ErrInvalidExecutablePath}
		}
		normalizedPath := strings.ToLower(path)
		if _, exists := seen[normalizedPath]; exists {
			return nil, &ExecutablePathError{Index: index, Path: path, Cause: ErrDuplicateExecutablePath}
		}
		seen[normalizedPath] = struct{}{}
	}

	rules := make([]nativeRule, 0, len(paths)*2)
	for _, path := range paths {
		rules = append(rules,
			nativeRule{
				direction: nativeDirectionOutbound, family: nativeAddressFamilyIPv4,
				protocol: nativeProtocolTCP, flags: nativeFlagFilter | nativeFlagOffline,
				executablePath: path,
			},
			nativeRule{
				direction: nativeDirectionOutbound, family: nativeAddressFamilyIPv4,
				protocol: nativeProtocolUDP, flags: nativeFlagFilter,
				executablePath: path,
			},
		)
	}
	return rules, nil
}

func isAbsoluteWindowsPath(path string) bool {
	if len(path) >= 3 && isASCIILetter(path[0]) && path[1] == ':' && path[2] == '\\' {
		return true
	}
	if !strings.HasPrefix(path, `\\`) {
		return false
	}
	if strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`) {
		return false
	}
	server, remainder, found := strings.Cut(strings.TrimPrefix(path, `\\`), `\`)
	if !found || server == "" {
		return false
	}
	share, _, _ := strings.Cut(remainder, `\`)
	return share != ""
}

func isASCIILetter(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

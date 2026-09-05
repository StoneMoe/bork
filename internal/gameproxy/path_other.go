//go:build !windows

package gameproxy

import (
	"io/fs"
	"path/filepath"
)

func canonicalPath(path string) (string, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolvedPath, err := filepath.EvalSymlinks(absolutePath)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolvedPath), nil
}

func isDirectoryReparsePoint(_ string, _ fs.DirEntry) (bool, error) {
	return false, nil
}

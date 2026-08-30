//go:build windows

package gameproxy

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

const initialFinalPathBufferSize = 512

func canonicalPath(path string) (canonical string, err error) {
	pathName, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", fmt.Errorf("encode path: %w", err)
	}
	handle, err := windows.CreateFile(
		pathName,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return "", fmt.Errorf("open path handle: %w", err)
	}
	defer func() {
		err = errors.Join(err, windows.CloseHandle(handle))
	}()

	canonical, err = finalPathFromHandle(handle)
	return canonical, err
}

func finalPathFromHandle(handle windows.Handle) (string, error) {
	bufferSize := uint32(initialFinalPathBufferSize)
	for {
		buffer := make([]uint16, bufferSize)
		length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], bufferSize, 0)
		if err != nil {
			return "", fmt.Errorf("get final path from handle: %w", err)
		}
		if length < bufferSize {
			return normalizeWindowsPath(windows.UTF16ToString(buffer[:length])), nil
		}
		bufferSize = length + 1
	}
}

func normalizeWindowsPath(path string) string {
	normalized := strings.ReplaceAll(path, "/", `\`)
	if strings.HasPrefix(normalized, `\\?\UNC\`) {
		normalized = `\\` + strings.TrimPrefix(normalized, `\\?\UNC\`)
	} else {
		normalized = strings.TrimPrefix(normalized, `\\?\`)
	}
	return strings.ToLower(filepath.Clean(normalized))
}

func isDirectoryReparsePoint(path string, _ fs.DirEntry) (bool, error) {
	pathName, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, fmt.Errorf("encode path: %w", err)
	}
	attributes, err := windows.GetFileAttributes(pathName)
	if err != nil {
		return false, fmt.Errorf("get file attributes: %w", err)
	}
	return attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 && attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0, nil
}

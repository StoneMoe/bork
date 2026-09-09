package logging

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const maxLogSize = 4 << 20

// Open opens the persistent application log and rotates one previous copy.
func Open(path string) (*os.File, error) {
	if path == "" {
		return nil, errors.New("log path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	if info, err := os.Stat(path); err == nil && info.Size() >= maxLogSize {
		previous := path + ".1"
		if err := os.Remove(previous); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("remove previous log: %w", err)
		}
		if err := os.Rename(path, previous); err != nil {
			return nil, fmt.Errorf("rotate application log: %w", err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect application log: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open application log: %w", err)
	}
	return file, nil
}

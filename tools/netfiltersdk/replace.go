package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type renameFunc func(string, string) error

type installer struct {
	rename renameFunc
}

type replacementResult struct {
	backupCleanupError error
}

func (sdkInstaller installer) ingest(lock manifest, archiveSource, destination string) (result replacementResult, returnErr error) {
	parent := filepath.Dir(destination)
	staging, err := os.MkdirTemp(parent, "."+filepath.Base(destination)+".stage-")
	if err != nil {
		return replacementResult{}, fmt.Errorf("create SDK staging directory: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, os.RemoveAll(staging)) }()

	if err := stageArchive(lock, archiveSource, staging); err != nil {
		return replacementResult{}, fmt.Errorf("stage SDK: %w", err)
	}
	if err := verifyInstallation(lock, staging); err != nil {
		return replacementResult{}, fmt.Errorf("verify staged SDK: %w", err)
	}
	result, err = replaceDirectory(staging, destination, sdkInstaller.rename)
	if err != nil {
		return replacementResult{}, fmt.Errorf("replace SDK directory: %w", err)
	}
	return result, nil
}

func replaceDirectory(staging, destination string, rename renameFunc) (replacementResult, error) {
	info, err := os.Lstat(destination)
	if errors.Is(err, os.ErrNotExist) {
		if err := rename(staging, destination); err != nil {
			return replacementResult{}, fmt.Errorf("install staged SDK: %w", err)
		}
		return replacementResult{}, nil
	}
	if err != nil {
		return replacementResult{}, fmt.Errorf("inspect existing SDK: %w", err)
	}
	if !info.IsDir() {
		return replacementResult{}, fmt.Errorf("existing SDK path is not a directory")
	}

	backup, err := reserveBackupPath(destination)
	if err != nil {
		return replacementResult{}, err
	}
	if err := rename(destination, backup); err != nil {
		return replacementResult{}, fmt.Errorf("move existing SDK to backup: %w", err)
	}
	if err := rename(staging, destination); err != nil {
		if restoreErr := rename(backup, destination); restoreErr != nil {
			return replacementResult{}, errors.Join(
				fmt.Errorf("install staged SDK: %w", err),
				fmt.Errorf("restore existing SDK from %s: %w", backup, restoreErr),
			)
		}
		return replacementResult{}, fmt.Errorf("install staged SDK: %w", err)
	}
	cleanupErr := os.RemoveAll(backup)
	return replacementResult{backupCleanupError: cleanupErr}, nil
}

func reserveBackupPath(destination string) (string, error) {
	backup, err := os.MkdirTemp(filepath.Dir(destination), "."+filepath.Base(destination)+".backup-")
	if err != nil {
		return "", fmt.Errorf("reserve SDK backup path: %w", err)
	}
	if err := os.Remove(backup); err != nil {
		return "", fmt.Errorf("prepare SDK backup path: %w", err)
	}
	return backup, nil
}

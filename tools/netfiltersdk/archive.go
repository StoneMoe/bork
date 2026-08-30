package main

import (
	"archive/zip"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
)

const (
	sha256HexLength      = sha256.Size * 2
	maxArchiveSize       = int64(1 << 30)
	maxExtractedFileSize = int64(512 << 20)
)

func stageArchive(manifest manifest, sourcePath, stagingPath string) error {
	archiveDirectory := filepath.Join(stagingPath, "archive")
	if err := os.MkdirAll(archiveDirectory, 0o755); err != nil {
		return fmt.Errorf("create staged archive directory: %w", err)
	}
	stagedArchive := filepath.Join(archiveDirectory, manifest.archive.name)
	if err := copyVerifiedArchive(sourcePath, stagedArchive, manifest.archive.digest); err != nil {
		return err
	}
	if err := extractLockedFiles(manifest.files, stagedArchive, stagingPath); err != nil {
		return err
	}
	return nil
}

func copyVerifiedArchive(sourcePath, destinationPath string, expected digest) (returnErr error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open archive %s: %w", sourcePath, err)
	}
	defer func() { returnErr = errors.Join(returnErr, source.Close()) }()
	info, err := source.Stat()
	if err != nil {
		return fmt.Errorf("stat archive %s: %w", sourcePath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("archive %s is not an ordinary file", sourcePath)
	}
	if info.Size() < 0 || info.Size() > maxArchiveSize {
		return fmt.Errorf("archive %s size %d exceeds limit %d", sourcePath, info.Size(), maxArchiveSize)
	}

	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create staged archive: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, destination.Close()) }()
	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(destination, hasher), io.LimitReader(source, info.Size()+1))
	if err != nil {
		return fmt.Errorf("copy archive: %w", err)
	}
	if written != info.Size() {
		return fmt.Errorf("archive changed while reading: copied %d bytes, expected %d", written, info.Size())
	}
	if actual := digestFromHash(hasher); actual != expected {
		return fmt.Errorf("archive sha256 mismatch")
	}
	return nil
}

func extractLockedFiles(files []fileLock, archivePath, stagingPath string) (returnErr error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open verified ZIP archive: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, reader.Close()) }()

	entries := make(map[string]*zip.File, len(reader.File))
	for _, entry := range reader.File {
		if _, exists := entries[entry.Name]; exists {
			return fmt.Errorf("ZIP entry %q is duplicated", entry.Name)
		}
		entries[entry.Name] = entry
	}
	for _, lockedFile := range files {
		entry, exists := entries[lockedFile.path.raw]
		if !exists {
			return fmt.Errorf("locked ZIP entry %q is missing", lockedFile.path.raw)
		}
		if !entry.Mode().IsRegular() {
			return fmt.Errorf("locked ZIP entry %q is not an ordinary file", lockedFile.path.raw)
		}
		if entry.UncompressedSize64 > uint64(maxExtractedFileSize) {
			return fmt.Errorf("locked ZIP entry %q exceeds size limit %d", lockedFile.path.raw, maxExtractedFileSize)
		}
		target := filepath.Join(stagingPath, filepath.FromSlash(lockedFile.path.raw))
		if err := extractFile(entry, target, lockedFile.digest); err != nil {
			return fmt.Errorf("extract %q: %w", lockedFile.path.raw, err)
		}
	}
	return nil
}

func extractFile(entry *zip.File, targetPath string, expected digest) (returnErr error) {
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}
	source, err := entry.Open()
	if err != nil {
		return fmt.Errorf("open ZIP entry: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, source.Close()) }()
	destination, err := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create staged file: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, destination.Close()) }()

	expectedSize := int64(entry.UncompressedSize64)
	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(destination, hasher), io.LimitReader(source, expectedSize+1))
	if err != nil {
		return fmt.Errorf("copy contents: %w", err)
	}
	if written != expectedSize {
		return fmt.Errorf("size mismatch: copied %d bytes, expected %d", written, expectedSize)
	}
	if actual := digestFromHash(hasher); actual != expected {
		return fmt.Errorf("sha256 mismatch")
	}
	mode := entry.Mode().Perm()
	if err := os.Chmod(targetPath, mode); err != nil {
		return fmt.Errorf("preserve file mode %o: %w", mode, err)
	}
	return nil
}

func verifyInstallation(manifest manifest, destination string) error {
	archivePath := filepath.Join(destination, "archive", manifest.archive.name)
	if err := verifyFile(archivePath, manifest.archive.digest, maxArchiveSize); err != nil {
		return fmt.Errorf("verify archive: %w", err)
	}
	for _, lockedFile := range manifest.files {
		installedPath := filepath.Join(destination, filepath.FromSlash(lockedFile.path.raw))
		if err := verifyFile(installedPath, lockedFile.digest, maxExtractedFileSize); err != nil {
			return fmt.Errorf("verify %q: %w", lockedFile.path.raw, err)
		}
	}
	return nil
}

func verifyFile(filePath string, expected digest, maximumSize int64) (returnErr error) {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open %s: %w", filePath, err)
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", filePath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not an ordinary file", filePath)
	}
	if info.Size() < 0 || info.Size() > maximumSize {
		return fmt.Errorf("%s size %d exceeds limit %d", filePath, info.Size(), maximumSize)
	}
	hasher := sha256.New()
	read, err := io.Copy(hasher, io.LimitReader(file, info.Size()+1))
	if err != nil {
		return fmt.Errorf("hash %s: %w", filePath, err)
	}
	if read != info.Size() {
		return fmt.Errorf("%s changed while reading", filePath)
	}
	if actual := digestFromHash(hasher); actual != expected {
		return fmt.Errorf("%s sha256 mismatch", filePath)
	}
	return nil
}

func digestFromHash(hasher hash.Hash) digest {
	var value digest
	copy(value[:], hasher.Sum(nil))
	return value
}

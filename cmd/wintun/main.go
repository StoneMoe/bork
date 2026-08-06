package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	wintunURL           = "https://www.wintun.net/builds/wintun-0.14.1.zip"
	wintunArchiveSize   = 750540
	wintunArchiveSHA256 = "07c256185d6ee3652e09fa55c0b673e2624b565e02c4b9091c79ca7d2f24ef51"
)

type asset struct {
	archivePath string
	outputPath  string
	size        int
	sha256      string
	data        []byte
}

var assets = []*asset{
	{archivePath: "wintun/bin/amd64/wintun.dll", outputPath: "amd64/wintun.dll", size: 427552, sha256: "e5da8447dc2c320edc0fc52fa01885c103de8c118481f683643cacc3220dafce"},
	{archivePath: "wintun/bin/arm64/wintun.dll", outputPath: "arm64/wintun.dll", size: 222488, sha256: "f7ba89005544be9d85231a9e0d5f23b2d15b3311667e2dad0debd344918a3f80"},
	{archivePath: "wintun/bin/x86/wintun.dll", outputPath: "386/wintun.dll", size: 550928, sha256: "d694fa46ab4cfebcb2632d094c7aa97278eef2f8052438621766d863ae98a931"},
	{archivePath: "wintun/LICENSE.txt", outputPath: "LICENSE.txt", size: 5431, sha256: "183adac21e7d96c508c8fd34d394b7b6708bc81564ad1bad61ab66143a008cd2"},
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	return prepareAssets(filepath.Join("internal", "peer", "wintun_generated"), assets, downloadArchive)
}

func prepareAssets(target string, files []*asset, download func() ([]byte, error)) error {
	if generatedAssetsValid(target, files) {
		return nil
	}
	unlock, err := lockAssetPreparation(target)
	if err != nil {
		return err
	}
	defer unlock()
	if generatedAssetsValid(target, files) {
		return nil
	}
	archive, err := download()
	if err != nil {
		return err
	}
	if err := extractAssets(archive, files); err != nil {
		return err
	}
	return installAssets(target, files)
}

func lockAssetPreparation(target string) (func(), error) {
	lock := target + ".lock"
	deadline := time.Now().Add(2 * time.Minute)
	for {
		unlock, acquired, err := tryAssetLock(lock)
		if err != nil {
			return nil, fmt.Errorf("lock generated Wintun assets: %w", err)
		}
		if acquired {
			return unlock, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("time out waiting for generated Wintun asset lock %s", lock)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func downloadArchive() ([]byte, error) {
	client := &http.Client{Timeout: time.Minute}
	response, err := client.Get(wintunURL)
	if err != nil {
		return nil, fmt.Errorf("download official Wintun 0.14.1: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download official Wintun 0.14.1: server returned %s", response.Status)
	}
	archive, err := io.ReadAll(io.LimitReader(response.Body, wintunArchiveSize+1))
	if err != nil {
		return nil, fmt.Errorf("read official Wintun 0.14.1 archive: %w", err)
	}
	if len(archive) != wintunArchiveSize || digest(archive) != wintunArchiveSHA256 {
		return nil, errors.New("official Wintun 0.14.1 archive failed fixed size/SHA-256 verification")
	}
	return archive, nil
}

func extractAssets(archive []byte, files []*asset) error {
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return fmt.Errorf("open official Wintun 0.14.1 archive: %w", err)
	}
	wanted := make(map[string]*asset, len(files))
	for _, file := range files {
		file.data = nil
		wanted[file.archivePath] = file
	}
	for _, file := range reader.File {
		target := wanted[file.Name]
		if target == nil {
			continue
		}
		if target.data != nil {
			return fmt.Errorf("official Wintun archive contains duplicate %s", file.Name)
		}
		if file.UncompressedSize64 != uint64(target.size) {
			return fmt.Errorf("%s has size %d, want %d", file.Name, file.UncompressedSize64, target.size)
		}
		input, err := file.Open()
		if err != nil {
			return fmt.Errorf("open %s: %w", file.Name, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(input, int64(target.size)+1))
		closeErr := input.Close()
		if readErr != nil {
			return fmt.Errorf("read %s: %w", file.Name, readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close %s: %w", file.Name, closeErr)
		}
		if len(data) != target.size || digest(data) != target.sha256 {
			return fmt.Errorf("%s failed fixed size/SHA-256 verification", file.Name)
		}
		target.data = data
	}
	for _, file := range files {
		if file.data == nil {
			return fmt.Errorf("official Wintun archive is missing %s", file.archivePath)
		}
	}
	return nil
}

func installAssets(target string, files []*asset) error {
	parent := filepath.Dir(target)
	info, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("locate package directory %s: %w", parent, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("package path %s is not a directory", parent)
	}
	staging, err := os.MkdirTemp(parent, ".wintun-generated-")
	if err != nil {
		return fmt.Errorf("create Wintun staging directory: %w", err)
	}
	defer os.RemoveAll(staging)
	for _, file := range files {
		path := filepath.Join(staging, filepath.FromSlash(file.outputPath))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, file.data, 0o644); err != nil {
			return err
		}
	}
	if !generatedAssetsValid(staging, files) {
		return errors.New("staged Wintun assets failed verification")
	}
	if generatedAssetsValid(target, files) {
		return nil
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("remove invalid generated Wintun assets: %w", err)
	}
	if err := os.Rename(staging, target); err != nil {
		return fmt.Errorf("publish generated Wintun assets: %w", err)
	}
	return nil
}

func generatedAssetsValid(root string, files []*asset) bool {
	wantedFiles := make(map[string]*asset, len(files))
	wantedDirectories := map[string]bool{".": true}
	for _, file := range files {
		path := filepath.Clean(filepath.FromSlash(file.outputPath))
		wantedFiles[path] = file
		for directory := filepath.Dir(path); directory != "."; directory = filepath.Dir(directory) {
			wantedDirectories[directory] = true
		}
	}
	count := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if !wantedDirectories[relative] {
				return errors.New("unexpected generated directory")
			}
			return nil
		}
		expected, ok := wantedFiles[relative]
		if !ok {
			return errors.New("unexpected generated file")
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() != int64(expected.size) {
			return errors.New("generated file size mismatch")
		}
		data, err := os.ReadFile(path)
		if err != nil || digest(data) != expected.sha256 {
			return errors.New("generated file SHA-256 mismatch")
		}
		count++
		return nil
	})
	return err == nil && count == len(wantedFiles)
}

func digest(data []byte) string {
	hash := sha256.Sum256(data)
	return fmt.Sprintf("%x", hash)
}

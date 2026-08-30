package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

type fixtureEntry struct {
	path string
	body []byte
	mode fs.FileMode
}

type sdkFixture struct {
	root        string
	manifest    string
	archive     string
	destination string
}

type fixtureManifest struct {
	Schema    int                  `json:"schema"`
	Version   string               `json:"version"`
	SourceURL string               `json:"source_url"`
	Archive   fixtureArchive       `json:"archive"`
	Platform  string               `json:"platform"`
	Files     []fixtureLockedEntry `json:"files"`
}

type fixtureArchive struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

type fixtureLockedEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func newSDKFixture(t *testing.T, zipEntries, lockedEntries []fixtureEntry) sdkFixture {
	t.Helper()

	root := t.TempDir()
	archivePath := filepath.Join(root, "source.zip")
	writeZIP(t, archivePath, zipEntries)
	archiveBytes, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("read fixture archive: %v", err)
	}

	files := make([]fixtureLockedEntry, 0, len(lockedEntries))
	for _, entry := range lockedEntries {
		files = append(files, fixtureLockedEntry{Path: entry.path, SHA256: digestBytes(entry.body)})
	}
	lock := fixtureManifest{
		Schema:    1,
		Version:   "test-version",
		SourceURL: "https://example.invalid/sdk.zip",
		Archive:   fixtureArchive{Name: "sdk.zip", SHA256: digestBytes(archiveBytes)},
		Platform:  "windows/amd64",
		Files:     files,
	}
	manifestBytes, err := json.Marshal(lock)
	if err != nil {
		t.Fatalf("marshal fixture manifest: %v", err)
	}
	manifestPath := filepath.Join(root, "sdk.lock.json")
	if err := os.WriteFile(manifestPath, manifestBytes, 0o600); err != nil {
		t.Fatalf("write fixture manifest: %v", err)
	}

	return sdkFixture{
		root:        root,
		manifest:    manifestPath,
		archive:     archivePath,
		destination: filepath.Join(root, "sdk"),
	}
}

func writeZIP(t *testing.T, archivePath string, entries []fixtureEntry) {
	t.Helper()

	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("create fixture archive: %v", err)
	}
	writer := zip.NewWriter(file)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.path, Method: zip.Deflate}
		header.SetMode(entry.mode)
		part, createErr := writer.CreateHeader(header)
		if createErr != nil {
			t.Fatalf("create ZIP entry %q: %v", entry.path, createErr)
		}
		if _, writeErr := part.Write(entry.body); writeErr != nil {
			t.Fatalf("write ZIP entry %q: %v", entry.path, writeErr)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close ZIP writer: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close fixture archive: %v", err)
	}
}

func digestBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func installOldDestination(t *testing.T, destination string) string {
	t.Helper()

	marker := filepath.Join(destination, "old-marker")
	if err := os.MkdirAll(destination, 0o755); err != nil {
		t.Fatalf("create old destination: %v", err)
	}
	if err := os.WriteFile(marker, []byte("old"), 0o600); err != nil {
		t.Fatalf("write old marker: %v", err)
	}
	return marker
}

type testCLI struct {
	cli
	stderr *bytes.Buffer
}

func newTestCLI(fixture sdkFixture) testCLI {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	return testCLI{
		cli: cli{
			stdout: stdout,
			stderr: stderr,
			workspace: workspace{
				manifestPath: fixture.manifest,
				destination:  fixture.destination,
			},
			rename: os.Rename,
		},
		stderr: stderr,
	}
}

func assertFileBody(t *testing.T, path string, want []byte) {
	t.Helper()
	got := mustReadFile(t, path)
	if !bytes.Equal(got, want) {
		t.Fatalf("%s body = %q, want %q", path, got, want)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

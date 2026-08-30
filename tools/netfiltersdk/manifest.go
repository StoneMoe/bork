package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
)

const supportedSchema = 1

type digest [32]byte

type entryPath struct {
	raw string
}

type manifest struct {
	version  string
	archive  archiveLock
	platform string
	files    []fileLock
}

type archiveLock struct {
	name   string
	digest digest
}

type fileLock struct {
	path   entryPath
	digest digest
}

type manifestJSON struct {
	Schema    int         `json:"schema"`
	Version   string      `json:"version"`
	SourceURL string      `json:"source_url"`
	Archive   archiveJSON `json:"archive"`
	Platform  string      `json:"platform"`
	Files     []fileJSON  `json:"files"`
}

type archiveJSON struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

type fileJSON struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func parseManifest(reader io.Reader) (manifest, error) {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	var document manifestJSON
	if err := decoder.Decode(&document); err != nil {
		return manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return manifest{}, fmt.Errorf("decode manifest: trailing JSON value")
		}
		return manifest{}, fmt.Errorf("decode manifest trailing data: %w", err)
	}
	if document.Schema != supportedSchema {
		return manifest{}, fmt.Errorf("manifest schema %d is unsupported", document.Schema)
	}
	if document.Version == "" || document.SourceURL == "" || document.Platform == "" {
		return manifest{}, fmt.Errorf("manifest version, source_url, and platform are required")
	}
	if len(document.Files) == 0 {
		return manifest{}, fmt.Errorf("manifest files must not be empty")
	}

	archiveName, err := parseEntryPath(document.Archive.Name)
	if err != nil || path.Base(document.Archive.Name) != document.Archive.Name {
		return manifest{}, fmt.Errorf("archive name %q must be a safe file name", document.Archive.Name)
	}
	archiveDigest, err := parseDigest(document.Archive.SHA256)
	if err != nil {
		return manifest{}, fmt.Errorf("archive sha256: %w", err)
	}

	files := make([]fileLock, 0, len(document.Files))
	seen := make(map[string]struct{}, len(document.Files))
	for index, rawFile := range document.Files {
		lockedPath, parseErr := parseEntryPath(rawFile.Path)
		if parseErr != nil {
			return manifest{}, fmt.Errorf("file %d path: %w", index, parseErr)
		}
		if _, exists := seen[lockedPath.raw]; exists {
			return manifest{}, fmt.Errorf("file %d path %q is duplicated", index, lockedPath.raw)
		}
		seen[lockedPath.raw] = struct{}{}
		lockedDigest, parseErr := parseDigest(rawFile.SHA256)
		if parseErr != nil {
			return manifest{}, fmt.Errorf("file %q sha256: %w", lockedPath.raw, parseErr)
		}
		files = append(files, fileLock{path: lockedPath, digest: lockedDigest})
	}

	return manifest{
		version:  document.Version,
		archive:  archiveLock{name: archiveName.raw, digest: archiveDigest},
		platform: document.Platform,
		files:    files,
	}, nil
}

func parseDigest(raw string) (digest, error) {
	if len(raw) != sha256HexLength {
		return digest{}, fmt.Errorf("must contain exactly %d lowercase hexadecimal characters", sha256HexLength)
	}
	for _, character := range raw {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return digest{}, fmt.Errorf("must contain exactly %d lowercase hexadecimal characters", sha256HexLength)
		}
	}
	decoded, err := hex.DecodeString(raw)
	if err != nil {
		return digest{}, fmt.Errorf("decode: %w", err)
	}
	var parsed digest
	copy(parsed[:], decoded)
	return parsed, nil
}

func parseEntryPath(raw string) (entryPath, error) {
	if raw == "" || raw == "." || raw == ".." {
		return entryPath{}, fmt.Errorf("entry path %q is empty or relative to a dot component", raw)
	}
	if strings.ContainsRune(raw, '\\') || path.IsAbs(raw) || hasDrivePrefix(raw) {
		return entryPath{}, fmt.Errorf("entry path %q must be a relative slash-separated path", raw)
	}
	for _, component := range strings.Split(raw, "/") {
		if component == "" || component == "." || component == ".." {
			return entryPath{}, fmt.Errorf("entry path %q contains an unsafe component", raw)
		}
	}
	if path.Clean(raw) != raw {
		return entryPath{}, fmt.Errorf("entry path %q is not canonical", raw)
	}
	return entryPath{raw: raw}, nil
}

func hasDrivePrefix(raw string) bool {
	return len(raw) >= 2 && ((raw[0] >= 'a' && raw[0] <= 'z') || (raw[0] >= 'A' && raw[0] <= 'Z')) && raw[1] == ':'
}

func (path entryPath) String() string {
	return path.raw
}

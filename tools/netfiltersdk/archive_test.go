package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStageArchive_verifies_archive_before_extracting_locked_files(t *testing.T) {
	// Given
	entry := fixtureEntry{path: "nfsdk/file.bin", body: []byte("contents"), mode: 0o644}
	fixture := newSDKFixture(t, []fixtureEntry{entry}, []fixtureEntry{entry})
	lock, err := loadManifest(fixture.manifest)
	if err != nil {
		t.Fatalf("load fixture manifest: %v", err)
	}
	archive, err := os.OpenFile(fixture.archive, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open archive for tampering: %v", err)
	}
	if _, err := archive.Write([]byte("tampered")); err != nil {
		t.Fatalf("tamper archive: %v", err)
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("close tampered archive: %v", err)
	}
	staging := filepath.Join(fixture.root, "staging")
	if err := os.Mkdir(staging, 0o755); err != nil {
		t.Fatalf("create staging directory: %v", err)
	}

	// When
	err = stageArchive(lock, fixture.archive, staging)

	// Then
	if err == nil {
		t.Fatal("stage archive succeeded, want error")
	}
	if _, statErr := os.Stat(filepath.Join(staging, filepath.FromSlash(entry.path))); !os.IsNotExist(statErr) {
		t.Fatalf("locked file was extracted before archive verification or stat failed: %v", statErr)
	}
}

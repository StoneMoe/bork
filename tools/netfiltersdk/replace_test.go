package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestIngest_restores_destination_when_install_rename_fails(t *testing.T) {
	// Given
	entry := fixtureEntry{path: "file.bin", body: []byte("new"), mode: 0o644}
	fixture := newSDKFixture(t, []fixtureEntry{entry}, []fixtureEntry{entry})
	oldMarker := installOldDestination(t, fixture.destination)
	manifestFile, err := os.Open(fixture.manifest)
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	manifest, err := parseManifest(manifestFile)
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if err := manifestFile.Close(); err != nil {
		t.Fatalf("close manifest: %v", err)
	}
	renameCalls := 0
	rename := func(oldPath, newPath string) error {
		renameCalls++
		if renameCalls == 2 {
			return errors.New("injected install rename failure")
		}
		return os.Rename(oldPath, newPath)
	}

	// When
	_, err = (installer{rename: rename}).ingest(manifest, fixture.archive, fixture.destination)

	// Then
	if err == nil {
		t.Fatal("ingest succeeded, want error")
	}
	assertFileBody(t, oldMarker, []byte("old"))
	if _, statErr := os.Stat(filepath.Join(fixture.destination, entry.path)); !os.IsNotExist(statErr) {
		t.Fatalf("new file exists or stat failed unexpectedly: %v", statErr)
	}
}

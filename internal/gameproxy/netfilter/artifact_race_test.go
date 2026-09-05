package netfilter

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestArtifactMaterializer_accepts_matching_race_winner(t *testing.T) {
	// Given
	contents := []byte("trusted artifact")
	materializer := newTestArtifactMaterializer(t.TempDir(), contents)
	materializer.publish = func(_, target string) error {
		if err := os.WriteFile(target, contents, 0o600); err != nil {
			return err
		}
		return fs.ErrExist
	}

	// When
	path, err := materializer.materialize(context.Background())

	// Then
	if err != nil {
		t.Fatal(err)
	}
	if path != materializer.targetPath() {
		t.Fatalf("materialized path = %q, want %q", path, materializer.targetPath())
	}
	assertNoTemporaryArtifacts(t, filepath.Dir(path))
}

func TestArtifactMaterializer_rejects_and_preserves_mismatched_race_winner(t *testing.T) {
	// Given
	materializer := newTestArtifactMaterializer(t.TempDir(), []byte("trusted artifact"))
	materializer.publish = func(_, target string) error {
		return errors.Join(os.WriteFile(target, []byte("race mismatch"), 0o600), fs.ErrExist)
	}

	// When
	_, err := materializer.materialize(context.Background())

	// Then
	assertArtifactIntegrityError(t, err)
	got, readErr := os.ReadFile(materializer.targetPath())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "race mismatch" {
		t.Fatalf("race winner contents = %q, want preserved mismatch", got)
	}
	assertNoTemporaryArtifacts(t, filepath.Dir(materializer.targetPath()))
}

func TestArtifactMaterializer_removes_temporary_file_when_publication_fails(t *testing.T) {
	// Given
	publishErr := errors.New("publish failed")
	materializer := newTestArtifactMaterializer(t.TempDir(), []byte("trusted artifact"))
	materializer.publish = func(_, _ string) error { return publishErr }

	// When
	_, err := materializer.materialize(context.Background())

	// Then
	if !errors.Is(err, publishErr) {
		t.Fatalf("materialize error = %v, want publication failure", err)
	}
	if _, statErr := os.Lstat(materializer.targetPath()); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("target stat error = %v, want os.ErrNotExist", statErr)
	}
	assertNoTemporaryArtifacts(t, filepath.Dir(materializer.targetPath()))
}

func assertNoTemporaryArtifacts(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "nfapi.dll" {
			t.Fatalf("unexpected temporary artifact %q", entry.Name())
		}
	}
}

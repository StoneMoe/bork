package netfilter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestArtifactMaterializer_rejects_bad_embedded_bytes_without_touching_cache(t *testing.T) {
	// Given
	cacheRoot := filepath.Join(t.TempDir(), "untouched")
	materializer := newTestArtifactMaterializer(cacheRoot, []byte("expected"))
	materializer.spec.contents = []byte("corrupt")

	// When
	_, err := materializer.materialize(context.Background())

	// Then
	assertArtifactIntegrityError(t, err)
	if _, statErr := os.Lstat(cacheRoot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("cache root stat error = %v, want os.ErrNotExist", statErr)
	}
}

func TestArtifactMaterializer_publishes_fresh_artifact_at_stable_path(t *testing.T) {
	// Given
	contents := []byte("trusted artifact")
	materializer := newTestArtifactMaterializer(t.TempDir(), contents)
	wantPath := materializer.targetPath()

	// When
	gotPath, err := materializer.materialize(context.Background())

	// Then
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != wantPath {
		t.Fatalf("materialized path = %q, want %q", gotPath, wantPath)
	}
	got, err := os.ReadFile(gotPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(contents) {
		t.Fatalf("materialized contents = %q, want %q", got, contents)
	}
	info, err := os.Lstat(gotPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("materialized mode = %o, want 600", info.Mode().Perm())
	}
}

func TestArtifactMaterializer_repeated_materialization_preserves_existing_file(t *testing.T) {
	// Given
	materializer := newTestArtifactMaterializer(t.TempDir(), []byte("trusted artifact"))
	path, err := materializer.materialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	// When
	repeatedPath, err := materializer.materialize(context.Background())

	// Then
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(repeatedPath)
	if err != nil {
		t.Fatal(err)
	}
	if repeatedPath != path || !os.SameFile(before, after) {
		t.Fatalf("repeat path = %q with same file %t, want %q with same file", repeatedPath, os.SameFile(before, after), path)
	}
}

func TestArtifactMaterializer_accepts_matching_existing_file(t *testing.T) {
	// Given
	contents := []byte("trusted artifact")
	materializer := newTestArtifactMaterializer(t.TempDir(), contents)
	writeExistingArtifact(t, materializer.targetPath(), contents)

	// When
	path, err := materializer.materialize(context.Background())

	// Then
	if err != nil {
		t.Fatal(err)
	}
	if path != materializer.targetPath() {
		t.Fatalf("materialized path = %q, want %q", path, materializer.targetPath())
	}
}

func TestArtifactMaterializer_rejects_and_preserves_mismatched_existing_file(t *testing.T) {
	// Given
	materializer := newTestArtifactMaterializer(t.TempDir(), []byte("trusted artifact"))
	path := materializer.targetPath()
	writeExistingArtifact(t, path, []byte("user-owned mismatch"))

	// When
	_, err := materializer.materialize(context.Background())

	// Then
	assertArtifactIntegrityError(t, err)
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "user-owned mismatch" {
		t.Fatalf("existing contents = %q, want preserved mismatch", got)
	}
}

func TestArtifactMaterializer_rejects_and_preserves_non_regular_existing_target(t *testing.T) {
	// Given
	materializer := newTestArtifactMaterializer(t.TempDir(), []byte("trusted artifact"))
	path := materializer.targetPath()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}

	// When
	_, err := materializer.materialize(context.Background())

	// Then
	assertArtifactIntegrityError(t, err)
	info, statErr := os.Lstat(path)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if !info.IsDir() {
		t.Fatalf("existing target mode = %v, want preserved directory", info.Mode())
	}
}

func TestArtifactMaterializer_pre_cancelled_context_touches_no_filesystem(t *testing.T) {
	// Given
	cacheRoot := filepath.Join(t.TempDir(), "untouched")
	materializer := newTestArtifactMaterializer(cacheRoot, []byte("trusted artifact"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// When
	_, err := materializer.materialize(ctx)

	// Then
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("materialize error = %v, want context.Canceled", err)
	}
	if _, statErr := os.Lstat(cacheRoot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("cache root stat error = %v, want os.ErrNotExist", statErr)
	}
}

func newTestArtifactMaterializer(cacheRoot string, contents []byte) artifactMaterializer {
	sum := sha256.Sum256(contents)
	return artifactMaterializer{
		cacheRoot: cacheRoot,
		spec: artifactSpec{
			version:  "test-version",
			filename: "nfapi.dll",
			digest:   hex.EncodeToString(sum[:]),
			contents: contents,
		},
		publish: os.Link,
	}
}

func writeExistingArtifact(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertArtifactIntegrityError(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrArtifactIntegrity) {
		t.Fatalf("materialize error = %v, want ErrArtifactIntegrity", err)
	}
	var integrityErr *ArtifactIntegrityError
	if !errors.As(err, &integrityErr) {
		t.Fatalf("materialize error type = %T, want *ArtifactIntegrityError", err)
	}
}

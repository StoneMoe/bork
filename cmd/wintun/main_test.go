package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrepareAssetsUsesValidCacheOffline(t *testing.T) {
	files := testAssets()
	target := filepath.Join(t.TempDir(), "generated")
	writeTestAssets(t, target, files)
	downloaded := false
	err := prepareAssets(target, files, func() ([]byte, error) {
		downloaded = true
		return nil, errors.New("offline")
	})
	if err != nil || downloaded {
		t.Fatalf("prepareAssets() error = %v, downloaded = %v", err, downloaded)
	}
}

func TestGeneratedAssetsRequireExactFilesSizeAndHash(t *testing.T) {
	files := testAssets()
	for name, mutate := range map[string]func(*testing.T, string){
		"extra file": func(t *testing.T, root string) { writeTestFile(t, filepath.Join(root, "extra"), []byte("extra")) },
		"extra directory": func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, "extra"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"missing file": func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, filepath.FromSlash(files[0].outputPath))); err != nil {
				t.Fatal(err)
			}
		},
		"wrong size": func(t *testing.T, root string) {
			writeTestFile(t, filepath.Join(root, filepath.FromSlash(files[0].outputPath)), []byte("short"))
		},
		"wrong hash": func(t *testing.T, root string) {
			writeTestFile(t, filepath.Join(root, filepath.FromSlash(files[0].outputPath)), []byte("other-data"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "generated")
			writeTestAssets(t, target, files)
			mutate(t, target)
			if generatedAssetsValid(target, files) {
				t.Fatal("invalid generated assets were accepted")
			}
		})
	}
}

func TestInstallAssetsReplacesInvalidCache(t *testing.T) {
	files := testAssets()
	target := filepath.Join(t.TempDir(), "generated")
	writeTestAssets(t, target, files)
	writeTestFile(t, filepath.Join(target, filepath.FromSlash(files[0].outputPath)), []byte("invalid"))
	if err := installAssets(target, files); err != nil {
		t.Fatal(err)
	}
	if !generatedAssetsValid(target, files) {
		t.Fatal("replacement assets are invalid")
	}
}

func TestFailedInstallPreservesValidCache(t *testing.T) {
	files := testAssets()
	target := filepath.Join(t.TempDir(), "generated")
	writeTestAssets(t, target, files)
	files[0].data = []byte("invalid staging data")
	if err := installAssets(target, files); err == nil {
		t.Fatal("invalid staging tree was published")
	}
	if !generatedAssetsValid(target, files) {
		t.Fatal("failed publish damaged the valid cache")
	}
}

func TestAssetPreparationLockSerializesPublishers(t *testing.T) {
	target := filepath.Join(t.TempDir(), "generated")
	unlock, err := lockAssetPreparation(target)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan func(), 1)
	errors := make(chan error, 1)
	go func() {
		nextUnlock, err := lockAssetPreparation(target)
		if err != nil {
			errors <- err
			return
		}
		acquired <- nextUnlock
	}()
	select {
	case nextUnlock := <-acquired:
		nextUnlock()
		unlock()
		t.Fatal("second publisher acquired the held lock")
	case err := <-errors:
		unlock()
		t.Fatal(err)
	case <-time.After(150 * time.Millisecond):
	}
	unlock()
	select {
	case nextUnlock := <-acquired:
		nextUnlock()
	case err := <-errors:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("second publisher did not acquire the released lock")
	}
}

func testAssets() []*asset {
	return []*asset{
		{outputPath: "amd64/wintun.dll", size: len("valid-data"), sha256: digest([]byte("valid-data")), data: []byte("valid-data")},
		{outputPath: "LICENSE.txt", size: len("license"), sha256: digest([]byte("license")), data: []byte("license")},
	}
}

func writeTestAssets(t *testing.T, root string, files []*asset) {
	t.Helper()
	for _, file := range files {
		writeTestFile(t, filepath.Join(root, filepath.FromSlash(file.outputPath)), file.data)
	}
	if !generatedAssetsValid(root, files) {
		t.Fatal("test assets are invalid")
	}
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

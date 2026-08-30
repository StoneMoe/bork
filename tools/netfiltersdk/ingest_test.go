package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCLI_ingest_installs_only_locked_entries_and_archive(t *testing.T) {
	// Given
	locked := []fixtureEntry{
		{path: "nfsdk/include/header.h", body: []byte("header"), mode: 0o644},
		{path: "nfsdk/bin/tool.exe", body: []byte("binary"), mode: 0o755},
	}
	fixture := newSDKFixture(t, append(locked, fixtureEntry{path: "unlisted.txt", body: []byte("ignore"), mode: 0o644}), locked)
	oldMarker := installOldDestination(t, fixture.destination)
	command := newTestCLI(fixture)

	// When
	exitCode := command.run([]string{"ingest", "-archive", fixture.archive})

	// Then
	if exitCode != 0 {
		t.Fatalf("ingest exit code = %d, stderr = %q", exitCode, command.stderr)
	}
	assertFileBody(t, filepath.Join(fixture.destination, locked[0].path), locked[0].body)
	assertFileBody(t, filepath.Join(fixture.destination, locked[1].path), locked[1].body)
	assertFileBody(t, filepath.Join(fixture.destination, "archive", "sdk.zip"), mustReadFile(t, fixture.archive))
	if _, err := os.Stat(filepath.Join(fixture.destination, "unlisted.txt")); !os.IsNotExist(err) {
		t.Fatalf("unlisted entry exists or stat failed unexpectedly: %v", err)
	}
	if _, err := os.Stat(oldMarker); !os.IsNotExist(err) {
		t.Fatalf("old destination marker remains or stat failed unexpectedly: %v", err)
	}
	info, err := os.Stat(filepath.Join(fixture.destination, locked[1].path))
	if err != nil {
		t.Fatalf("stat executable: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("executable mode = %o, want 755", info.Mode().Perm())
	}
}

func TestCLI_verify_accepts_untampered_installation(t *testing.T) {
	// Given
	locked := []fixtureEntry{{path: "nfsdk/file.bin", body: []byte("contents"), mode: 0o644}}
	fixture := newSDKFixture(t, locked, locked)
	command := newTestCLI(fixture)
	if exitCode := command.run([]string{"ingest", "-archive", fixture.archive}); exitCode != 0 {
		t.Fatalf("prepare installation: exit %d, stderr = %q", exitCode, command.stderr)
	}
	command.stderr.Reset()

	// When
	exitCode := command.run([]string{"verify"})

	// Then
	if exitCode != 0 {
		t.Fatalf("verify exit code = %d, stderr = %q", exitCode, command.stderr)
	}
}

func TestCLI_verify_rejects_tampered_file(t *testing.T) {
	// Given
	locked := []fixtureEntry{{path: "nfsdk/file.bin", body: []byte("contents"), mode: 0o644}}
	fixture := newSDKFixture(t, locked, locked)
	command := newTestCLI(fixture)
	if exitCode := command.run([]string{"ingest", "-archive", fixture.archive}); exitCode != 0 {
		t.Fatalf("prepare installation: exit %d, stderr = %q", exitCode, command.stderr)
	}
	installedFile := filepath.Join(fixture.destination, locked[0].path)
	if err := os.WriteFile(installedFile, []byte("tampered"), 0o644); err != nil {
		t.Fatalf("tamper installed file: %v", err)
	}
	command.stderr.Reset()

	// When
	exitCode := command.run([]string{"verify"})

	// Then
	if exitCode == 0 {
		t.Fatal("verify succeeded, want failure")
	}
}

func TestCLI_ingest_succeeds_when_archive_is_inside_destination(t *testing.T) {
	// Given
	locked := []fixtureEntry{{path: "nfsdk/file.bin", body: []byte("contents"), mode: 0o644}}
	fixture := newSDKFixture(t, locked, locked)
	installOldDestination(t, fixture.destination)
	archiveInside := filepath.Join(fixture.destination, "archive", "sdk.zip")
	if err := os.MkdirAll(filepath.Dir(archiveInside), 0o755); err != nil {
		t.Fatalf("create old archive directory: %v", err)
	}
	if err := os.Rename(fixture.archive, archiveInside); err != nil {
		t.Fatalf("move archive inside destination: %v", err)
	}
	originalArchive := mustReadFile(t, archiveInside)
	command := newTestCLI(fixture)

	// When
	exitCode := command.run([]string{"ingest", "-archive", archiveInside})

	// Then
	if exitCode != 0 {
		t.Fatalf("ingest exit code = %d, stderr = %q", exitCode, command.stderr)
	}
	assertFileBody(t, filepath.Join(fixture.destination, locked[0].path), locked[0].body)
	assertFileBody(t, filepath.Join(fixture.destination, "archive", "sdk.zip"), originalArchive)
}

func TestCLI_verify_rejects_tampered_archive(t *testing.T) {
	// Given
	locked := []fixtureEntry{{path: "nfsdk/file.bin", body: []byte("contents"), mode: 0o644}}
	fixture := newSDKFixture(t, locked, locked)
	command := newTestCLI(fixture)
	if exitCode := command.run([]string{"ingest", "-archive", fixture.archive}); exitCode != 0 {
		t.Fatalf("prepare installation: exit %d, stderr = %q", exitCode, command.stderr)
	}
	installedArchive := filepath.Join(fixture.destination, "archive", "sdk.zip")
	file, err := os.OpenFile(installedArchive, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open installed archive: %v", err)
	}
	if _, err := file.Write([]byte("tampered")); err != nil {
		t.Fatalf("tamper installed archive: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close installed archive: %v", err)
	}
	command.stderr.Reset()

	// When
	exitCode := command.run([]string{"verify"})

	// Then
	if exitCode == 0 {
		t.Fatal("verify succeeded, want failure")
	}
}

func TestIngest_rejects_invalid_archives_without_changing_destination(t *testing.T) {
	tests := []struct {
		name   string
		create func(*testing.T) sdkFixture
	}{
		{
			name: "wrong archive digest",
			create: func(t *testing.T) sdkFixture {
				entry := fixtureEntry{path: "file.bin", body: []byte("good"), mode: 0o644}
				fixture := newSDKFixture(t, []fixtureEntry{entry}, []fixtureEntry{entry})
				file, err := os.OpenFile(fixture.archive, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatalf("open archive for tampering: %v", err)
				}
				if _, err := file.Write([]byte("tampered")); err != nil {
					t.Fatalf("tamper archive: %v", err)
				}
				if err := file.Close(); err != nil {
					t.Fatalf("close tampered archive: %v", err)
				}
				return fixture
			},
		},
		{
			name: "missing locked entry",
			create: func(t *testing.T) sdkFixture {
				present := fixtureEntry{path: "present.bin", body: []byte("present"), mode: 0o644}
				missing := fixtureEntry{path: "missing.bin", body: []byte("missing"), mode: 0o644}
				return newSDKFixture(t, []fixtureEntry{present}, []fixtureEntry{present, missing})
			},
		},
		{
			name: "duplicate ZIP entry",
			create: func(t *testing.T) sdkFixture {
				entry := fixtureEntry{path: "file.bin", body: []byte("same"), mode: 0o644}
				return newSDKFixture(t, []fixtureEntry{entry, entry}, []fixtureEntry{entry})
			},
		},
		{
			name: "locked file digest mismatch",
			create: func(t *testing.T) sdkFixture {
				archiveEntry := fixtureEntry{path: "file.bin", body: []byte("archive"), mode: 0o644}
				lockedEntry := fixtureEntry{path: "file.bin", body: []byte("locked"), mode: 0o644}
				return newSDKFixture(t, []fixtureEntry{archiveEntry}, []fixtureEntry{lockedEntry})
			},
		},
		{
			name: "non regular locked entry",
			create: func(t *testing.T) sdkFixture {
				entry := fixtureEntry{path: "file.bin", body: []byte("target"), mode: os.ModeSymlink | 0o777}
				return newSDKFixture(t, []fixtureEntry{entry}, []fixtureEntry{entry})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Given
			fixture := test.create(t)
			oldMarker := installOldDestination(t, fixture.destination)
			command := newTestCLI(fixture)

			// When
			exitCode := command.run([]string{"ingest", "-archive", fixture.archive})

			// Then
			if exitCode == 0 {
				t.Fatal("ingest succeeded, want failure")
			}
			assertFileBody(t, oldMarker, []byte("old"))
		})
	}
}

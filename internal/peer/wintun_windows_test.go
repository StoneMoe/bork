//go:build windows

package peer

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWintunRuntimePathUsesProgramDataKnownFolder(t *testing.T) {
	programData, err := programDataPath()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ProgramData", t.TempDir())
	path, err := wintunRuntimePath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(programData, "Bork", "Wintun-"+wintunVersion, runtime.GOARCH)
	if path != want {
		t.Fatalf("runtime path = %q, want %q", path, want)
	}
}

func TestWintunRuntimeSecurityDescriptor(t *testing.T) {
	descriptor, err := windows.SecurityDescriptorFromString(wintunRuntimeSDDL)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil || !owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
		t.Fatalf("owner = %v, %v", owner, err)
	}
	group, _, err := descriptor.Group()
	if err != nil || !group.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
		t.Fatalf("group = %v, %v", group, err)
	}
	control, _, err := descriptor.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 || descriptor.String() != wintunRuntimeSDDL {
		t.Fatalf("security descriptor = %q, control=%x, error=%v", descriptor.String(), control, err)
	}
}

func TestLockedRuntimeFileDeniesWriteAndDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wintun.dll")
	data := []byte("locked Wintun test")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	file, info, err := openLockedRuntimeFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	if matches, err := lockedRuntimeFileMatches(file, info, digest); err != nil || !matches {
		t.Fatalf("locked hash match = %v, %v", matches, err)
	}
	if err := os.WriteFile(path, []byte("changed"), 0o600); err == nil {
		t.Fatal("locked file allowed writing")
	}
	replacement := filepath.Join(filepath.Dir(path), "replacement.dll")
	if err := os.WriteFile(replacement, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err == nil {
		t.Fatal("locked file allowed replacement")
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
		t.Fatalf("write after unlock: %v", err)
	}
}

func TestLockedRuntimeDirectoryDeniesReplacement(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "current")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := openLockedRuntimeDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(path, "child")
	if err := os.WriteFile(child, []byte("materialize"), 0o600); err != nil {
		lock.Close()
		t.Fatalf("write inside locked directory: %v", err)
	}
	if err := os.Remove(child); err != nil {
		lock.Close()
		t.Fatalf("remove child inside locked directory: %v", err)
	}
	moved := filepath.Join(parent, "moved")
	if err := os.Rename(path, moved); err == nil {
		lock.Close()
		t.Fatal("locked directory allowed rename/replacement")
	}
	if err := os.Remove(path); err == nil {
		lock.Close()
		t.Fatal("locked directory allowed deletion")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, moved); err != nil {
		t.Fatalf("rename after unlock: %v", err)
	}
}

func TestRuntimeDirectoryLockRejectsExistingDeleteHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(path16, windows.DELETE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	lock, err := openLockedRuntimeDirectory(path)
	if err == nil {
		lock.Close()
		t.Fatal("directory lock accepted an existing delete handle")
	}
}

func TestWintunProtectedRuntimeACL(t *testing.T) {
	elevated, err := processIsElevated()
	if err != nil {
		t.Fatal(err)
	}
	if !elevated {
		t.Skip("ACL application requires an elevated test process")
	}
	descriptor, err := windows.SecurityDescriptorFromString(wintunRuntimeSDDL)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "protected")
	if err := ensureProtectedDirectory(directory, descriptor); err != nil {
		t.Fatal(err)
	}
	data := []byte("protected Wintun test")
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	path := filepath.Join(directory, "wintun.dll")
	if err := writeRuntimeFile(path, data, digest, descriptor); err != nil {
		t.Fatal(err)
	}
	if err := verifyRuntimePathSecurity(path, descriptor); err != nil {
		t.Fatal(err)
	}
}

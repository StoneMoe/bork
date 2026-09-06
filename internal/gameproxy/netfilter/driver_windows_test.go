//go:build windows && amd64 && game_proxy

package netfilter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestDriverServiceClassification(t *testing.T) {
	path := `C:\Windows\System32\drivers\bork_netfilter_demo.sys`
	for _, test := range []struct {
		name      string
		image     string
		kind      uint32
		start     uint32
		status    uint32
		wantState driverState
		wantError error
	}{
		{"running", path, windows.SERVICE_KERNEL_DRIVER, windows.SERVICE_SYSTEM_START, windows.SERVICE_RUNNING, driverRunning, nil},
		{"stopped", `\??\` + path, windows.SERVICE_KERNEL_DRIVER, windows.SERVICE_DEMAND_START, windows.SERVICE_STOPPED, driverStopped, nil},
		{"system root", `\SystemRoot\System32\drivers\bork_netfilter_demo.sys`, windows.SERVICE_KERNEL_DRIVER, windows.SERVICE_SYSTEM_START, windows.SERVICE_RUNNING, driverRunning, nil},
		{"SDK system relative", `system32\drivers\bork_netfilter_demo.sys`, windows.SERVICE_KERNEL_DRIVER, windows.SERVICE_SYSTEM_START, windows.SERVICE_RUNNING, driverRunning, nil},
		{"SDK system relative stopped", `SYSTEM32\DRIVERS\BORK_NETFILTER_DEMO.SYS`, windows.SERVICE_KERNEL_DRIVER, windows.SERVICE_SYSTEM_START, windows.SERVICE_STOPPED, driverStopped, nil},
		{"case", strings.ToLower(path), windows.SERVICE_KERNEL_DRIVER, windows.SERVICE_AUTO_START, windows.SERVICE_RUNNING, driverRunning, nil},
		{"start pending", path, windows.SERVICE_KERNEL_DRIVER, 1, windows.SERVICE_START_PENDING, driverPending, nil},
		{"stop pending", path, windows.SERVICE_KERNEL_DRIVER, 1, windows.SERVICE_STOP_PENDING, driverPending, nil},
		{"continue pending", path, windows.SERVICE_KERNEL_DRIVER, 1, windows.SERVICE_CONTINUE_PENDING, driverPending, nil},
		{"pause pending", path, windows.SERVICE_KERNEL_DRIVER, 1, windows.SERVICE_PAUSE_PENDING, driverPending, nil},
		{"disabled", path, windows.SERVICE_KERNEL_DRIVER, windows.SERVICE_DISABLED, windows.SERVICE_RUNNING, driverMissing, errDriverDisabled},
		{"invalid start", path, windows.SERVICE_KERNEL_DRIVER, 55, windows.SERVICE_RUNNING, driverMissing, errDriverConflict},
		{"user service", path, windows.SERVICE_WIN32_OWN_PROCESS, 1, windows.SERVICE_RUNNING, driverMissing, errDriverConflict},
		{"filesystem driver", path, windows.SERVICE_FILE_SYSTEM_DRIVER, 1, windows.SERVICE_RUNNING, driverMissing, errDriverConflict},
		{"other name", `C:\Windows\System32\drivers\other.sys`, windows.SERVICE_KERNEL_DRIVER, 1, windows.SERVICE_RUNNING, driverMissing, errDriverConflict},
		{"other directory", `C:\Temp\bork_netfilter_demo.sys`, windows.SERVICE_KERNEL_DRIVER, 1, windows.SERVICE_RUNNING, driverMissing, errDriverConflict},
		{"relative", `drivers\bork_netfilter_demo.sys`, windows.SERVICE_KERNEL_DRIVER, 1, windows.SERVICE_RUNNING, driverMissing, errDriverConflict},
		{"system relative other name", `system32\drivers\other.sys`, windows.SERVICE_KERNEL_DRIVER, 1, windows.SERVICE_RUNNING, driverMissing, errDriverConflict},
		{"system relative traversal", `system32\drivers\..\bork_netfilter_demo.sys`, windows.SERVICE_KERNEL_DRIVER, 1, windows.SERVICE_RUNNING, driverMissing, errDriverConflict},
		{"system relative arguments", `system32\drivers\bork_netfilter_demo.sys --option`, windows.SERVICE_KERNEL_DRIVER, 1, windows.SERVICE_RUNNING, driverMissing, errDriverConflict},
		{"system relative stream", `system32\drivers\bork_netfilter_demo.sys:stream`, windows.SERVICE_KERNEL_DRIVER, 1, windows.SERVICE_RUNNING, driverMissing, errDriverConflict},
		{"environment", `%SystemRoot%\System32\drivers\bork_netfilter_demo.sys`, windows.SERVICE_KERNEL_DRIVER, 1, windows.SERVICE_RUNNING, driverMissing, errDriverConflict},
		{"arguments", path + " --option", windows.SERVICE_KERNEL_DRIVER, 1, windows.SERVICE_RUNNING, driverMissing, errDriverConflict},
		{"alternate stream", path + ":stream", windows.SERVICE_KERNEL_DRIVER, 1, windows.SERVICE_RUNNING, driverMissing, errDriverConflict},
		{"paused", path, windows.SERVICE_KERNEL_DRIVER, 1, windows.SERVICE_PAUSED, driverMissing, errDriverConflict},
		{"unknown status", path, windows.SERVICE_KERNEL_DRIVER, 1, 999, driverMissing, errDriverConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, err := classifyDriverService(test.kind, test.start, test.image, test.status, path)
			if state != test.wantState || !errors.Is(err, test.wantError) {
				t.Fatalf("state, error = %v, %v; want %v, %v", state, err, test.wantState, test.wantError)
			}
		})
	}
}

func TestDriverServiceErrors(t *testing.T) {
	for _, test := range []struct {
		osError error
		want    error
	}{
		{windows.ERROR_ACCESS_DENIED, errDriverInaccessible},
		{windows.ERROR_SERVICE_DISABLED, errDriverDisabled},
		{windows.ERROR_SERVICE_MARKED_FOR_DELETE, errDriverRemovalPending},
	} {
		err := driverServiceError("fake query", test.osError)
		if !errors.Is(err, test.want) || !errors.Is(err, test.osError) {
			t.Fatalf("error %v does not preserve both causes", err)
		}
	}
}

func TestDriverRegistrationError(t *testing.T) {
	err := driverRegistrationError(-1, 0)
	var errno syscall.Errno
	if err == nil || errors.As(err, &errno) || errors.Is(err, syscall.Errno(0)) {
		t.Fatalf("SDK failure with zero last-error must not expose an errno: %v", err)
	}
	if !strings.Contains(err.Error(), "NF_STATUS=-1") {
		t.Fatalf("SDK status missing: %v", err)
	}
	err = driverRegistrationError(-1, windows.ERROR_ACCESS_DENIED)
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) || !strings.Contains(err.Error(), "NF_STATUS=-1") {
		t.Fatalf("nonzero last-error or SDK status missing: %v", err)
	}
}

func TestDriverSecurityDescriptors(t *testing.T) {
	for _, test := range []struct {
		name      string
		sddl      string
		protected bool
		root      bool
		valid     bool
	}{
		{"product directory", driverDirectorySDDL, true, false, true},
		{"payload", driverFileSDDL, true, false, true},
		{"system owned", "O:SYG:SYD:P(A;;FA;;;SY)(A;;FRFX;;;BU)", true, false, true},
		{"Windows ancestor", "O:" + trustedInstallerSID + "D:(A;;FA;;;SY)(A;;FRFX;;;BU)", false, false, true},
		{"untrusted owner", "O:BUD:P(A;;FRFX;;;BU)", true, false, false},
		{"unprotected", "O:BAD:(A;;FA;;;BA)(A;;FRFX;;;BU)", true, false, false},
		{"null DACL", "O:BAD:NO_ACCESS_CONTROL", false, false, false},
		{"user write", "O:BAD:P(A;;FW;;;BU)", true, false, false},
		{"user all", "O:BAD:P(A;;GA;;;BU)", true, false, false},
		{"user delete", "O:BAD:P(A;;SD;;;BU)", true, false, false},
		{"user change DACL", "O:BAD:P(A;;WD;;;BU)", true, false, false},
		{"user change owner", "O:BAD:P(A;;WO;;;BU)", true, false, false},
		{"user delete children", "O:BAD:P(A;;0x40;;;BU)", true, false, false},
		{"root child creation", "O:BAD:P(A;;0x6;;;AU)(A;;FRFX;;;BU)", false, true, true},
		{"non-root child creation", "O:BAD:P(A;;0x6;;;AU)(A;;FRFX;;;BU)", false, false, false},
		{"root delete children", "O:BAD:P(A;;0x40;;;AU)", false, true, false},
		{"creator inherit only", "O:BAD:P(A;OICIIO;FA;;;CO)(A;;FRFX;;;BU)", false, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(test.sddl)
			if err != nil {
				t.Fatal(err)
			}
			err = verifyDriverSecurity(sd, test.protected, test.root)
			if test.valid && err != nil {
				t.Fatal(err)
			}
			if !test.valid && !errors.Is(err, errDriverConflict) {
				t.Fatalf("want conflict, got %v", err)
			}
		})
	}
}

func TestDriverEmbeddedPayloads(t *testing.T) {
	for name, payload := range map[string]struct {
		contents []byte
		digest   string
	}{
		"DLL":     {embeddedNetFilterDLL, netFilterDLLSHA256},
		"SYS":     {embeddedNetFilterDriver, netFilterDriverSHA256},
		"license": {[]byte(embeddedNetFilterLicense), netFilterLicenseSHA256},
	} {
		if got := driverDigest(payload.contents); got != payload.digest {
			t.Fatalf("%s digest %s, want %s", name, got, payload.digest)
		}
	}
}

func TestDriverFileContents(t *testing.T) {
	contents := []byte("original temporary payload")
	path := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := verifyDriverContents(file, int64(len(contents)), driverDigest(contents)); err != nil {
		t.Fatal(err)
	}
	if err := verifyDriverContents(file, int64(len(contents)), driverDigest([]byte("different"))); !errors.Is(err, errDriverConflict) {
		t.Fatalf("hash mismatch: %v", err)
	}
	if err := verifyDriverContents(file, int64(len(contents)+1), driverDigest(contents)); !errors.Is(err, errDriverConflict) {
		t.Fatalf("size mismatch: %v", err)
	}
	if err := verifyDriverContents(file, int64(len(contents)), driverDigest(contents)); err != nil {
		t.Fatalf("repeated verification must reset the file offset: %v", err)
	}
}

func TestDriverRejectsHardLinksAndWrongObjectType(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "payload")
	if err := os.WriteFile(path, []byte("temporary"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, filepath.Join(directory, "second-name")); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path      string
		directory bool
	}{
		{path, false},
		{path, true},
		{directory, false},
	} {
		file, err := openDriverObject(test.path, test.directory, false, false)
		if file != nil {
			file.Close()
		}
		if !errors.Is(err, errDriverConflict) {
			t.Fatalf("want conflict for %s, got %v", test.path, err)
		}
	}
}

func TestDriverDoesNotOverwriteUnknownFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing")
	before := []byte("foreign file")
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	files := &driverFiles{files: make(map[string]*os.File)}
	err := files.writePayload(context.Background(), path, []byte("new"), driverDigest([]byte("new")))
	if !errors.Is(err, errDriverConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("foreign file changed: %q, %v", after, err)
	}
}

func TestDriverRejectsReparsePoint(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	link := filepath.Join(directory, "link")
	if err := os.WriteFile(target, []byte("temporary"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
			t.Skip("temporary symlink creation requires Windows Developer Mode")
		}
		t.Fatal(err)
	}
	file, err := openDriverObject(link, false, false, false)
	if file != nil {
		file.Close()
	}
	if !errors.Is(err, errDriverConflict) {
		t.Fatalf("want reparse-point conflict, got %v", err)
	}
}

func TestDriverLockContention(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, driverLockName)
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	name := windows.StringToUTF16Ptr(path)
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	files := &driverFiles{directory: directory}
	err = files.lock(context.Background())
	windows.CloseHandle(handle)
	if !errors.Is(err, errDriverInstallInProgress) || !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("want installation-in-progress sharing violation, got %v", err)
	}
	// Releasing the handle releases the lock; no permanent singleton survives.
	handle, err = windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	windows.CloseHandle(handle)
}

func TestDriverPinsBlockRenameWriteAndDelete(t *testing.T) {
	directory := t.TempDir()
	parent := filepath.Join(directory, "parent")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	pinnedDirectory, err := pinDriverObject(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer pinnedDirectory.Close()
	if err := os.Rename(parent, filepath.Join(directory, "renamed")); err == nil {
		t.Fatal("directory could be renamed while pinned")
	}
	path := filepath.Join(parent, "payload")
	if err := os.WriteFile(path, []byte("temporary"), 0600); err != nil {
		t.Fatalf("directory pin must still allow creating its children: %v", err)
	}
	pinnedFile, err := pinDriverObject(path)
	if err != nil {
		t.Fatal(err)
	}
	defer pinnedFile.Close()
	writer, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err == nil {
		writer.Close()
		t.Fatal("file could be opened for writing while pinned")
	}
	if err := os.Rename(path, filepath.Join(parent, "renamed")); err == nil {
		t.Fatal("file could be renamed while pinned")
	}
	if err := os.Remove(path); err == nil {
		t.Fatal("file could be deleted while pinned")
	}
	pinnedFile.Close()
	pinnedDirectory.Close()
	if err := os.Rename(parent, filepath.Join(directory, "renamed")); err != nil {
		t.Fatalf("released pins still block renaming: %v", err)
	}
}

func TestDriverCancellationBeforeMutation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	directory := t.TempDir()
	files := &driverFiles{directory: filepath.Join(directory, "absent"), files: make(map[string]*os.File)}
	for name, operation := range map[string]func() error{
		"install":   func() error { return installDriver(ctx) },
		"directory": func() error { return files.createDirectory(ctx) },
		"lock":      func() error { return files.lock(ctx) },
		"payload": func() error {
			return files.writePayload(ctx, filepath.Join(directory, "payload"), nil, driverDigest(nil))
		},
		"register": func() error { return files.registerDriver(ctx) },
	} {
		if err := operation(); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("canceled operations changed temporary directory: %v, %v", entries, err)
	}
}

type cancelAfterDriverBootstrapCheck struct {
	context.Context
	cancel  context.CancelFunc
	checked bool
}

func (ctx *cancelAfterDriverBootstrapCheck) Err() error {
	if !ctx.checked {
		ctx.checked = true
		ctx.cancel()
		return nil
	}
	return ctx.Context.Err()
}

func TestDriverDirectoryBootstrapCompletesAfterCancellation(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("protected-owner temporary fixture requires an already-elevated token")
	}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &cancelAfterDriverBootstrapCheck{Context: base, cancel: cancel}
	files := &driverFiles{directory: filepath.Join(t.TempDir(), "product"), files: make(map[string]*os.File)}
	defer files.close()
	if err := files.createDirectory(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("bootstrap must honor cancellation after completing ownership: %v", err)
	}
	marker := files.files[filepath.Join(files.directory, driverMarkerName)]
	if marker == nil {
		t.Fatal("cancellation stranded a directory without its pinned ownership marker")
	}
	if err := verifyDriverContents(marker, int64(len(driverMarker)), driverDigest([]byte(driverMarker))); err != nil {
		t.Fatal(err)
	}
	if err := files.lock(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("lock must honor cancellation after ownership bootstrap: %v", err)
	}
	entries, err := os.ReadDir(files.directory)
	if err != nil || len(entries) != 1 || entries[0].Name() != driverMarkerName {
		t.Fatalf("bootstrap must create only ownership metadata: %v, %v", entries, err)
	}
}

func TestDriverDirectoryBootstrapDoesNotAdoptUnknownDirectory(t *testing.T) {
	directory := t.TempDir()
	files := &driverFiles{directory: directory, files: make(map[string]*os.File)}
	defer files.close()
	if err := files.createDirectory(context.Background()); !errors.Is(err, errDriverInstallInProgress) {
		t.Fatalf("existing directory must not be adopted: %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 || files.product {
		t.Fatalf("bootstrap modified an unknown directory: %v, %v", entries, err)
	}
}

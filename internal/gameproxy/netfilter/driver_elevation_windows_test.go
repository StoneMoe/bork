//go:build windows && amd64 && game_proxy

package netfilter

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestDriverHelperInvalidDispatch(t *testing.T) {
	pid := strconv.Itoa(os.Getpid())
	event := driverHelperEventPrefix + strings.Repeat("a", 64)
	for _, args := range [][]string{
		nil, {}, {"--prepare-driver"}, {"--prepare-driver", pid},
		{"--prepare-driver", pid, event, "extra"}, {"--other", pid, event},
		{"--prepare-driver", "0", event}, {"--prepare-driver", "-1", event},
		{"--prepare-driver", "+1", event}, {"--prepare-driver", "01", event},
		{"--prepare-driver", " 1", event}, {"--prepare-driver", "4294967296", event},
		{"--prepare-driver", pid, "Global\\" + event},
		{"--prepare-driver", pid, event + "a"}, {"--prepare-driver", pid, event[:len(event)-1]},
		{"--prepare-driver", pid, driverHelperEventPrefix + strings.Repeat("A", 64)},
		{"--prepare-driver", pid, event[:len(event)-1] + "g"},
		{"--prepare-driver", pid, event + "\x00"},
		{"--elevate-driver"}, {"--elevate-driver", pid, event},
		{"--elevate-driver", pid, event, strings.Repeat("a", 64), "0", "1", "2", "3"},
		{"--elevate-driver", pid, event, strings.Repeat("A", 64), "1", "2", "3", "4"},
	} {
		if code := RunDriverHelper(args); code != int(windows.ERROR_INVALID_PARAMETER) {
			t.Fatalf("args %q: code %d", args, code)
		}
	}
	if got, name, err := parseDriverHelperArgs([]string{"--prepare-driver", pid, event}); err != nil || got != uint32(os.Getpid()) || name != event {
		t.Fatalf("valid parse = %d, %q, %v", got, name, err)
	}
}

func TestDriverHelperCancelEvent(t *testing.T) {
	event, name, err := createDriverCancelEvent()
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(event)
	other, otherName, err := createDriverCancelEvent()
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(other)
	if name == otherName {
		t.Fatal("cancel event names are reused")
	}
	opened, err := windows.OpenEvent(windows.SYNCHRONIZE, false, windows.StringToUTF16Ptr(name))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(opened)
	if status, err := windows.WaitForSingleObject(opened, 0); err != nil || status != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("event was not initially nonsignaled: %d, %v", status, err)
	}
	if err := windows.SetEvent(event); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if status, err := windows.WaitForSingleObject(opened, 0); err != nil || status != windows.WAIT_OBJECT_0 {
			t.Fatalf("manual reset cancel event lost signal: %d, %v", status, err)
		}
	}
	// Query via a separate READ_CONTROL handle: the launch handle intentionally
	// requests only SYNCHRONIZE and EVENT_MODIFY_STATE.
	securityHandle, err := windows.OpenEvent(windows.READ_CONTROL, false, windows.StringToUTF16Ptr(name))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(securityHandle)
	sd, err := windows.GetSecurityInfo(securityHandle, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil || acl.AceCount != 3 {
		t.Fatalf("unexpected event DACL: %v, %v", acl, err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	for i := uint32(0); i < 3; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			t.Fatal(err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		want := uint32(windows.SYNCHRONIZE)
		if i == 0 {
			want |= windows.EVENT_MODIFY_STATE
			if sid.String() != user.User.Sid.String() {
				t.Fatal("cancel permission is not assigned to parent user")
			}
		} else if !sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) && !sid.IsWellKnown(windows.WinLocalSystemSid) {
			t.Fatal("missing administrator/SYSTEM synchronization access")
		}
		if uint32(ace.Mask) != want || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Fatalf("unexpected event access mask: %#x", ace.Mask)
		}
	}
}

func TestDriverHelperRequiresLiveUncancelledParent(t *testing.T) {
	event, name, err := createDriverCancelEvent()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--prepare-driver", strconv.Itoa(os.Getpid()), name}
	install := func(context.Context) error { t.Fatal("invalid lifetime reached installer"); return nil }
	if err := windows.SetEvent(event); err != nil {
		t.Fatal(err)
	}
	if code := runDriverHelper(args, install); code != int(windows.ERROR_CANCELLED) {
		t.Fatalf("late consent after cancellation: %d", code)
	}
	windows.CloseHandle(event)
	if code := runDriverHelper(args, install); code != int(windows.ERROR_CANCELLED) {
		t.Fatalf("missing event: %d", code)
	}
	args[1] = "4294967295"
	if code := runDriverHelper(args, install); code != int(windows.ERROR_CANCELLED) {
		t.Fatalf("missing parent: %d", code)
	}
}

func TestDriverHelperMonitorsCancellation(t *testing.T) {
	event, name, err := createDriverCancelEvent()
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(event)
	entered := make(chan struct{})
	result := make(chan int, 1)
	go func() {
		result <- runDriverHelper([]string{"--prepare-driver", strconv.Itoa(os.Getpid()), name}, func(ctx context.Context) error {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	select {
	case <-entered:
	case code := <-result:
		t.Fatalf("installer was not reached: %d", code)
	case <-time.After(time.Second):
		t.Fatal("helper did not start")
	}
	if err := windows.SetEvent(event); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-result:
		if code != int(windows.ERROR_CANCELLED) {
			t.Fatalf("canceled helper returned %d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("helper did not monitor cancellation")
	}
}

func TestDriverHelperMonitorsParentExit(t *testing.T) {
	if os.Getenv("BORK_HELPER_TEST_PARENT") == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	command, stdin := driverHelperTestParent(t)
	event, name, err := createDriverCancelEvent()
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(event)
	args := []string{"--prepare-driver", strconv.Itoa(command.Process.Pid), name}
	code := runDriverHelper(args, func(ctx context.Context) error {
		stdin.Close()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
			return errors.New("parent exit was not observed")
		}
	})
	if code != int(windows.ERROR_CANCELLED) {
		t.Fatalf("parent exit returned %d", code)
	}
	// The exited process object is still held by command until Wait, so a late
	// helper must reject it even when OpenProcess can still succeed.
	if code := runDriverHelper(args, func(context.Context) error { t.Fatal("exited parent reached installer"); return nil }); code != int(windows.ERROR_CANCELLED) {
		t.Fatalf("late consent after parent exit: %d", code)
	}
}

func driverHelperTestParent(t *testing.T) (*exec.Cmd, io.WriteCloser) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestDriverHelperMonitorsParentExit$")
	command.Env = append(os.Environ(), "BORK_HELPER_TEST_PARENT=1")
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		stdin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stdin.Close()
		command.Wait()
	})
	return command, stdin
}

func TestDriverHelperLaunchCancellationRetainsPins(t *testing.T) {
	materializer := helperTestMaterializer(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan string, 1)
	consent := make(chan struct{})
	launched := make(chan io.WriteCloser, 1)
	result := make(chan error, 1)
	go func() {
		result <- launchDriverHelper(ctx, materializer, func(command *exec.Cmd) error {
			if command.Path != materializer.targetPath() {
				return errors.New("unexpected executable path")
			}
			entered <- command.Args[3]
			<-consent
			command.Path = os.Args[0]
			command.Args = []string{command.Path, "-test.run=^TestDriverHelperMonitorsParentExit$"}
			command.Env = append(os.Environ(), "BORK_HELPER_TEST_PARENT=1")
			stdin, err := command.StdinPipe()
			if err != nil {
				return err
			}
			launched <- stdin
			return command.Start()
		})
	}()
	var name string
	select {
	case name = <-entered:
	case err := <-result:
		t.Fatalf("did not reach launch: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("did not reach launch")
	}
	cancel()
	event, err := windows.OpenEvent(windows.SYNCHRONIZE, false, windows.StringToUTF16Ptr(name))
	if err != nil {
		t.Fatal(err)
	}
	status, err := windows.WaitForSingleObject(event, 1000)
	windows.CloseHandle(event)
	if err != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatalf("cancel was not signaled during UAC: %d, %v", status, err)
	}
	close(consent)
	stdin := <-launched
	defer stdin.Close()
	select {
	case err := <-result:
		t.Fatalf("launch did not reap the live process: %v", err)
	default:
	}
	file, err := os.OpenFile(materializer.targetPath(), os.O_WRONLY, 0)
	if err == nil {
		file.Close()
		t.Fatal("cancellation released the executable before helper exit")
	}
	// An already-canceled launch must keep the event open for late consent.
	code := runDriverHelper([]string{"--prepare-driver", strconv.Itoa(os.Getpid()), name}, func(context.Context) error {
		t.Fatal("late consent reached driver mutation")
		return nil
	})
	if code != int(windows.ERROR_CANCELLED) {
		t.Fatalf("late helper returned %d", code)
	}
	stdin.Close()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("late result = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("helper process was not reaped")
	}
	if event, err := windows.OpenEvent(windows.SYNCHRONIZE, false, windows.StringToUTF16Ptr(name)); err == nil {
		windows.CloseHandle(event)
		t.Fatal("cancel event leaked after helper exit")
	}
	file, err = os.OpenFile(materializer.targetPath(), os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("executable pin leaked after helper exit: %v", err)
	}
	file.Close()
}

func driverGuardianTestMaterializer(t *testing.T, root string) artifactMaterializer {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	materializer := helperTestMaterializer(root)
	materializer.spec.contents = contents
	materializer.spec.digest = driverDigest(contents)
	return materializer
}

func driverGuardianTestCommand(command *exec.Cmd) {
	command.Args = append([]string{command.Path, "-test.run=^TestDriverGuardianHandoff$", "--"}, command.Args[1:]...)
	command.Env = append(os.Environ(), "BORK_DRIVER_GUARDIAN_ROLE=guardian")
}

func TestDriverGuardianRejectsInvalidHandoff(t *testing.T) {
	for _, scenario := range []struct {
		name string
		code int
	}{
		{"missing", int(windows.ERROR_INVALID_PARAMETER)},
		{"extra", int(windows.ERROR_INVALID_PARAMETER)},
		{"non-inherited", int(windows.ERROR_INVALID_PARAMETER)},
		{"wrong parent", int(windows.ERROR_INVALID_PARAMETER)},
		{"wrong event", int(windows.ERROR_INVALID_PARAMETER)},
		{"wrong file", driverHelperConflict},
		{"wrong ancestor", driverHelperConflict},
		{"digest", driverHelperConflict},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			materializer := driverGuardianTestMaterializer(t, t.TempDir())
			var child *exec.Cmd
			err := launchDriverHelper(context.Background(), materializer, func(command *exec.Cmd) error {
				child = command
				driverGuardianTestCommand(command)
				command.Env = append(command.Env, "BORK_DRIVER_GUARDIAN_CASE="+scenario.name)
				return command.Start()
			})
			if err == nil || child == nil || child.ProcessState == nil || child.ProcessState.ExitCode() != scenario.code {
				t.Fatalf("invalid %s handoff: command=%v, error=%v", scenario.name, child, err)
			}
		})
	}
}

// The subprocesses are this test binary, not the driver helper entry point.
// The guardian exercises production validation and reaping with a fake UAC
// callback; the fake worker only exits. No ShellExecute or installer is called.
func TestDriverGuardianHandoff(t *testing.T) {
	switch os.Getenv("BORK_DRIVER_GUARDIAN_ROLE") {
	case "worker":
		os.Exit(int(windows.ERROR_CANCELLED))
	case "guardian":
		var args []string
		for i, argument := range os.Args {
			if argument == "--" {
				args = os.Args[i+1:]
				break
			}
		}
		scenario := os.Getenv("BORK_DRIVER_GUARDIAN_CASE")
		switch scenario {
		case "missing":
			args = args[:len(args)-1]
		case "extra":
			args = append(args, "1")
		case "non-inherited":
			value, _ := strconv.ParseUint(args[len(args)-1], 10, 64)
			if err := windows.SetHandleInformation(windows.Handle(value), windows.HANDLE_FLAG_INHERIT, 0); err != nil {
				t.Fatal(err)
			}
		case "wrong parent":
			args[1] = strconv.Itoa(os.Getpid())
		case "wrong event":
			args[5], args[6] = args[6], args[5]
		case "wrong file":
			args[6], args[len(args)-1] = args[len(args)-1], args[6]
		case "wrong ancestor":
			args[6], args[7] = args[7], args[6]
		case "digest":
			args[3] = strings.Repeat("0", 64)
		}
		code := runDriverGuardian(args, func(path string, pid uint32, event string) (windows.Handle, error) {
			if scenario != "" {
				return 0, errors.New("invalid handoff reached elevation")
			}
			fmt.Fprintln(os.Stdout, "READY", os.Getpid(), event)
			var consent [1]byte
			if _, err := io.ReadFull(os.Stdin, consent[:]); err != nil {
				return 0, err
			}
			// Consent arrives only after the host has observed the original GUI
			// exit. Its process object is still alive via the inherited handle.
			code := runDriverHelper([]string{"--prepare-driver", strconv.FormatUint(uint64(pid), 10), event}, func(context.Context) error {
				return errors.New("late consent reached installation")
			})
			if code != int(windows.ERROR_CANCELLED) {
				return 0, fmt.Errorf("late worker result %d", code)
			}
			worker := exec.Command(path, "-test.run=^TestDriverGuardianHandoff$")
			worker.Env = append(os.Environ(), "BORK_DRIVER_GUARDIAN_ROLE=worker")
			if err := worker.Start(); err != nil {
				return 0, err
			}
			handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(worker.Process.Pid))
			worker.Process.Release()
			return handle, err
		})
		os.Exit(code)
	case "gui":
		materializer := driverGuardianTestMaterializer(t, os.Getenv("BORK_DRIVER_GUARDIAN_CACHE"))
		err := launchDriverHelper(context.Background(), materializer, func(command *exec.Cmd) error {
			driverGuardianTestCommand(command)
			command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := command.Start(); err != nil {
				return err
			}
			quit, err := windows.OpenEvent(windows.SYNCHRONIZE, false, windows.StringToUTF16Ptr(os.Getenv("BORK_DRIVER_GUARDIAN_QUIT")))
			if err != nil {
				return err
			}
			if _, err := windows.WaitForSingleObject(quit, 10000); err != nil {
				return err
			}
			// Deliberately bypass every defer and Wait, releasing every GUI handle.
			os.Exit(0)
			return nil
		})
		fmt.Fprintln(os.Stdout, "ERROR", err)
		os.Exit(driverHelperFailed)
	}

	root := t.TempDir()
	quit, quitName, err := createDriverCancelEvent()
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(quit)
	gui := exec.Command(os.Args[0], "-test.run=^TestDriverGuardianHandoff$")
	gui.Env = append(os.Environ(), "BORK_DRIVER_GUARDIAN_ROLE=gui", "BORK_DRIVER_GUARDIAN_CACHE="+root, "BORK_DRIVER_GUARDIAN_QUIT="+quitName)
	consentInput, consent, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer consentInput.Close()
	gui.Stdin = consentInput
	output, err := gui.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := gui.Start(); err != nil {
		t.Fatal(err)
	}
	consentInput.Close()
	defer func() {
		windows.SetEvent(quit)
		consent.Close()
		if gui.ProcessState == nil {
			gui.Wait()
		}
	}()
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(output).ReadString('\n')
		ready <- line
	}()
	var line string
	select {
	case line = <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("guardian did not reach fake UAC")
	}
	var marker, eventName string
	var guardianPID uint32
	if _, err := fmt.Sscan(line, &marker, &guardianPID, &eventName); err != nil || marker != "READY" {
		t.Fatalf("guardian failed before UAC: %q", line)
	}
	guardian, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, guardianPID)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(guardian)
	defer func() {
		consent.Close()
		windows.WaitForSingleObject(guardian, 5000)
	}()
	if err := windows.SetEvent(quit); err != nil {
		t.Fatal(err)
	}
	if err := gui.Wait(); err != nil {
		t.Fatalf("fake GUI exit: %v", err)
	}
	if status, err := windows.WaitForSingleObject(guardian, 100); err != nil || status != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("guardian exited during pending UAC after GUI death: %d, %v", status, err)
	}
	materializer := driverGuardianTestMaterializer(t, root)
	path := materializer.targetPath()
	if file, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil {
		file.Close()
		t.Fatal("GUI exit released inherited executable write exclusion")
	}
	if err := os.Remove(path); err == nil {
		t.Fatal("GUI exit released inherited executable delete exclusion")
	}
	parent := filepath.Dir(path)
	if err := os.Rename(parent, parent+"-moved"); err == nil {
		t.Fatal("GUI exit released inherited ancestor pin")
	}
	event, err := windows.OpenEvent(windows.SYNCHRONIZE, false, windows.StringToUTF16Ptr(eventName))
	if err != nil {
		t.Fatalf("GUI exit destroyed cancel event: %v", err)
	}
	windows.CloseHandle(event)
	if _, err := consent.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if status, err := windows.WaitForSingleObject(guardian, 5000); err != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatalf("guardian did not reap late worker: %d, %v", status, err)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(guardian, &code); err != nil || code != uint32(windows.ERROR_CANCELLED) {
		t.Fatalf("guardian did not preserve worker exit code: %d, %v", code, err)
	}
	if event, err := windows.OpenEvent(windows.SYNCHRONIZE, false, windows.StringToUTF16Ptr(eventName)); err == nil {
		windows.CloseHandle(event)
		t.Fatal("cancel event leaked after guardian exit")
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("guardian exit did not release executable pin: %v", err)
	}
	if err := os.Rename(parent, parent+"-moved"); err != nil {
		t.Fatalf("guardian exit did not release ancestor pin: %v", err)
	}
}

func TestDriverHelperWin32Results(t *testing.T) {
	event, name, err := createDriverCancelEvent()
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(event)
	args := []string{"--prepare-driver", strconv.Itoa(os.Getpid()), name}
	for _, errno := range []syscall.Errno{
		1, windows.ERROR_ACCESS_DENIED, windows.ERROR_INVALID_IMAGE_HASH,
		windows.ERROR_MOD_NOT_FOUND, windows.ERROR_DISK_FULL,
		driverHelperConflict, driverHelperWin32Mask,
	} {
		code := runDriverHelper(args, func(context.Context) error {
			return fmt.Errorf("fake installer: %w", driverRegistrationError(-1, errno))
		})
		wantCode := driverHelperWin32 | int(errno)
		if code != wantCode || driverHelperExitCode(errno) != wantCode {
			t.Fatalf("Win32 error %d: exit code %#x, want %#x", errno, code, wantCode)
		}
		got := driverHelperResult(uint32(code))
		if !errors.Is(got, errno) || !strings.Contains(got.Error(), fmt.Sprintf("Win32 error %d", errno)) {
			t.Fatalf("Win32 error %d lost in round trip: %v", errno, got)
		}
	}
}

func TestDriverHelperResults(t *testing.T) {
	for _, err := range []error{nil, windows.ERROR_CANCELLED, errDriverConflict, errDriverInstallInProgress, errDriverDisabled, errDriverRemovalPending, errDriverInaccessible} {
		if got := driverHelperResult(uint32(driverHelperExitCode(err))); !errors.Is(got, err) {
			t.Fatalf("result round trip for %v: %v", err, got)
		}
		if err != nil {
			wrapped := fmt.Errorf("%w: %w", err, windows.ERROR_ACCESS_DENIED)
			if got := driverHelperExitCode(wrapped); got != driverHelperExitCode(err) {
				t.Fatalf("domain error must take precedence over Win32 error: %v returned %#x", wrapped, got)
			}
		}
	}
	for _, err := range []error{
		errors.New("failure"), syscall.Errno(0), fmt.Errorf("SDK status failed: %w", syscall.Errno(0)),
		driverRegistrationError(-1, 0), syscall.Errno(0x10000), syscall.Errno(0x80070005),
		syscall.Errno(0xC0000005), syscall.Errno(0x100000005),
	} {
		if got := driverHelperExitCode(err); got != driverHelperFailed {
			t.Fatalf("failure without an encodable Win32 error returned %#x: %v", got, err)
		}
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		if got := driverHelperExitCode(err); got != int(windows.ERROR_CANCELLED) {
			t.Fatalf("cancellation returned %d", got)
		}
	}
	for _, code := range []uint32{
		1, 2, uint32(windows.ERROR_ACCESS_DENIED), uint32(windows.ERROR_INVALID_PARAMETER),
		99999, driverHelperFailed, driverHelperWin32, 0x80070005, 0xC0000005, 0xC0000409, 0xFFFFFFFF,
	} {
		got := driverHelperResult(code)
		var errno syscall.Errno
		if got == nil || errors.As(got, &errno) || !strings.Contains(got.Error(), fmt.Sprintf("exit code %d", code)) {
			t.Fatalf("untagged or invalid helper exit %#x must remain a generic failure: %v", code, got)
		}
	}
	if size := unsafe.Sizeof(driverShellExecuteInfo{}); size != 112 {
		t.Fatalf("SHELLEXECUTEINFOW size = %d", size)
	}
}

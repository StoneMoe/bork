//go:build windows && amd64 && game_proxy

package netfilter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	driverHelperEventPrefix = `Local\Bork.NetFilter.Prepare.`
	driverHelperConflict    = 0x2001
	driverHelperFailed      = 0x2002
	driverHelperBusy        = 0x2003
	driverHelperDisabled    = 0x2004
	driverHelperRemoving    = 0x2005
	driverHelperDenied      = 0x2006
	// Tag 16-bit Win32 errors; raw exits, NTSTATUS and HRESULT values are not
	// this protocol. Never truncate larger errors into a different Win32 code.
	driverHelperWin32     = 0xB04B0000
	driverHelperWin32Mask = 0xFFFF
)

func createDriverCancelEvent() (windows.Handle, string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return 0, "", err
	}
	// An over-the-shoulder administrator is not the original user. Both BA
	// and SYSTEM need SYNCHRONIZE, but only the parent user needs to signal.
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;0x100002;;;" + user.User.Sid.String() + ")(A;;0x100000;;;BA)(A;;0x100000;;;SY)")
	if err != nil {
		return 0, "", err
	}
	attributes := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return 0, "", err
	}
	name := driverHelperEventPrefix + hex.EncodeToString(nonce[:])
	event, err := windows.CreateEventEx(attributes, windows.StringToUTF16Ptr(name), windows.CREATE_EVENT_MANUAL_RESET,
		windows.EVENT_MODIFY_STATE|windows.SYNCHRONIZE)
	runtime.KeepAlive(attributes)
	if err != nil {
		if event != 0 {
			windows.CloseHandle(event)
		}
		return 0, "", fmt.Errorf("create helper cancellation event: %w", err)
	}
	return event, name, nil
}

func launchDriverHelper(ctx context.Context, materializer artifactMaterializer, start func(*exec.Cmd) error) error {
	helper, err := pinDriverHelper(ctx, materializer)
	if err != nil {
		return err
	}
	defer helper.close()
	event, name, err := createDriverCancelEvent()
	if err != nil {
		return err
	}
	signaled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		windows.SetEvent(event)
		close(signaled)
	})
	defer func() {
		if !stop() {
			<-signaled
		}
		windows.CloseHandle(event)
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	parent, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(os.Getpid()))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(parent)
	sources := []windows.Handle{parent, event}
	for _, file := range helper.files {
		sources = append(sources, windows.Handle(file.Fd()))
	}
	inherited := make([]syscall.Handle, 0, len(sources))
	defer func() {
		for _, handle := range inherited {
			windows.CloseHandle(windows.Handle(handle))
		}
	}()
	args := []string{"--elevate-driver", strconv.Itoa(os.Getpid()), name, materializer.spec.digest}
	for _, source := range sources {
		var handle windows.Handle
		if err := windows.DuplicateHandle(windows.CurrentProcess(), source, windows.CurrentProcess(), &handle, 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
			return err
		}
		inherited = append(inherited, syscall.Handle(handle))
		args = append(args, strconv.FormatUint(uint64(handle), 10))
	}
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return err
	}
	// CreateProcess transfers all the existing share-denying handles atomically.
	// The guardian can outlive the GUI, including while UAC is still pending.
	command := exec.Command(helper.path, args...)
	command.Dir = system
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, AdditionalInheritedHandles: inherited}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := start(command); err != nil {
		return fmt.Errorf("start driver guardian: %w", err)
	}
	for _, handle := range inherited {
		windows.CloseHandle(windows.Handle(handle))
	}
	inherited = nil
	// Do not use CommandContext: canceling the GUI must never kill the guardian
	// and release its pins while the elevation broker still has a pending launch.
	err = command.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return driverHelperResult(uint32(exit.ExitCode()))
	}
	return err
}

func executeDriverHelper(command *exec.Cmd) error {
	return command.Start()
}

var (
	driverGetHandleInformation = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetHandleInformation")
	driverCompareObjectHandles = windows.NewLazySystemDLL("kernelbase.dll").NewProc("CompareObjectHandles")
)

func runDriverGuardian(args []string, elevate func(string, uint32, string) (windows.Handle, error)) int {
	if len(args) < 8 || len(args) > 262 || args[0] != "--elevate-driver" {
		return int(windows.ERROR_INVALID_PARAMETER)
	}
	pid, eventName, err := parseDriverHelperArgs([]string{"--prepare-driver", args[1], args[2]})
	if err != nil || len(args[3]) != 64 || strings.Trim(args[3], "0123456789abcdef") != "" {
		return int(windows.ERROR_INVALID_PARAMETER)
	}
	handles := make([]windows.Handle, 0, len(args)-4)
	seen := make(map[windows.Handle]bool, len(args)-4)
	for _, argument := range args[4:] {
		value, err := strconv.ParseUint(argument, 10, 64)
		handle := windows.Handle(value)
		if err != nil || value == 0 || handle == windows.InvalidHandle || strconv.FormatUint(value, 10) != argument || seen[handle] {
			return int(windows.ERROR_INVALID_PARAMETER)
		}
		var flags uint32
		ok, _, _ := driverGetHandleInformation.Call(uintptr(handle), uintptr(unsafe.Pointer(&flags)))
		if ok == 0 || flags != windows.HANDLE_FLAG_INHERIT {
			return int(windows.ERROR_INVALID_PARAMETER)
		}
		seen[handle] = true
		handles = append(handles, handle)
	}
	parent, event := handles[0], handles[1]
	defer windows.CloseHandle(parent)
	defer windows.CloseHandle(event)
	helper := &pinnedDriverHelper{}
	for _, handle := range handles[2:] {
		helper.files = append(helper.files, os.NewFile(uintptr(handle), "inherited driver helper pin"))
	}
	defer helper.close()
	for _, handle := range handles {
		if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
			return int(windows.ERROR_INVALID_PARAMETER)
		}
	}
	if actualPID, err := windows.GetProcessId(parent); err != nil || actualPID != pid {
		return int(windows.ERROR_INVALID_PARAMETER)
	}
	namedEvent, err := windows.OpenEvent(windows.SYNCHRONIZE, false, windows.StringToUTF16Ptr(eventName))
	if err != nil {
		return int(windows.ERROR_INVALID_PARAMETER)
	}
	same, _, _ := driverCompareObjectHandles.Call(uintptr(event), uintptr(namedEvent))
	windows.CloseHandle(namedEvent)
	if same == 0 {
		return int(windows.ERROR_INVALID_PARAMETER)
	}
	helper.path, err = os.Executable()
	if err != nil {
		return driverHelperFailed
	}
	volume := filepath.VolumeName(helper.path)
	if len(volume) != 2 || volume[1] != ':' || !filepath.IsAbs(helper.path) || filepath.Clean(helper.path) != helper.path {
		return driverHelperConflict
	}
	path := volume + `\`
	if windows.GetDriveType(windows.StringToUTF16Ptr(path)) != windows.DRIVE_FIXED {
		return driverHelperConflict
	}
	components := strings.Split(strings.TrimPrefix(helper.path, path), `\`)
	inherited := helper.files
	if len(inherited) != len(components)+1 {
		return int(windows.ERROR_INVALID_PARAMETER)
	}
	for i, file := range inherited {
		if err := verifyHelperObject(file, path, i < len(components)); err != nil {
			return driverHelperConflict
		}
		// Independently pin the same objects, not just handles that happen to be
		// readable. The inherited originals remain open throughout validation.
		pin, err := pinDriverObject(path)
		if err != nil {
			return driverHelperExitCode(err)
		}
		helper.files = append(helper.files, pin)
		original, err := file.Stat()
		if err != nil {
			return driverHelperConflict
		}
		current, err := pin.Stat()
		if err != nil || !os.SameFile(original, current) {
			return driverHelperConflict
		}
		if i == len(components) {
			if err := verifyDriverContents(file, original.Size(), args[3]); err != nil {
				return driverHelperConflict
			}
			break
		}
		path = filepath.Join(path, components[i])
	}
	status, err := windows.WaitForMultipleObjects([]windows.Handle{parent, event}, false, 0)
	if err != nil || status != uint32(windows.WAIT_TIMEOUT) {
		return int(windows.ERROR_CANCELLED)
	}
	// Only this process's verified executable can be elevated. Keep the original
	// GUI process object (preventing PID reuse), event, and all pins until the
	// worker exits. Parent exit/cancellation must NOT interrupt this wait.
	process, err := elevate(helper.path, pid, eventName)
	if err != nil {
		return driverHelperExitCode(err)
	}
	defer windows.CloseHandle(process)
	if _, err := windows.WaitForSingleObject(process, windows.INFINITE); err != nil {
		return driverHelperFailed
	}
	var code uint32
	if err := windows.GetExitCodeProcess(process, &code); err != nil {
		return driverHelperFailed
	}
	return int(code)
}

// Layout of SHELLEXECUTEINFOW on the only supported architecture, amd64.
type driverShellExecuteInfo struct {
	size       uint32
	mask       uint32
	window     windows.Handle
	verb       *uint16
	file       *uint16
	parameters *uint16
	directory  *uint16
	show       int32
	instance   windows.Handle
	idList     uintptr
	class      *uint16
	classKey   windows.Handle
	hotKey     uint32
	icon       windows.Handle
	process    windows.Handle
}

var shellExecuteDriverHelper = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")

func elevateDriverWorker(path string, parentPID uint32, event string) (windows.Handle, error) {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return 0, err
	}
	file, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	parameters := "--prepare-driver " + strconv.FormatUint(uint64(parentPID), 10) + " " + event
	info := driverShellExecuteInfo{
		mask: 0x00000040 | 0x00000100, // SEE_MASK_NOCLOSEPROCESS | SEE_MASK_NOASYNC
		verb: windows.StringToUTF16Ptr("runas"), file: file,
		parameters: windows.StringToUTF16Ptr(parameters), directory: windows.StringToUTF16Ptr(system),
		show: windows.SW_HIDE,
	}
	info.size = uint32(unsafe.Sizeof(info))
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED|windows.COINIT_DISABLE_OLE1DDE); err != nil && err != syscall.Errno(1) { // S_FALSE also requires CoUninitialize.
		return 0, fmt.Errorf("initialize helper launch COM apartment: %w", err)
	}
	defer windows.CoUninitialize()
	ok, _, callErr := shellExecuteDriverHelper.Call(uintptr(unsafe.Pointer(&info)))
	runtime.KeepAlive(info)
	if ok == 0 {
		if info.process != 0 {
			windows.CloseHandle(info.process)
		}
		if callErr == syscall.Errno(0) {
			return 0, errors.New("netfilter: ShellExecuteExW failed without an error code")
		}
		return 0, fmt.Errorf("request driver helper elevation: %w", callErr)
	}
	if info.process == 0 {
		return 0, errors.New("netfilter: ShellExecuteExW did not return a helper process")
	}
	return info.process, nil
}

// RunDriverHelper accepts os.Args[1:] from the standalone, pure-Go helper.
// Invalid dispatch cannot reach process/event opening or driver mutation.
func RunDriverHelper(args []string) int {
	if len(args) != 0 && args[0] == "--elevate-driver" {
		return runDriverGuardian(args, elevateDriverWorker)
	}
	return runDriverHelper(args, installDriver)
}

func runDriverHelper(args []string, install func(context.Context) error) int {
	pid, eventName, err := parseDriverHelperArgs(args)
	if err != nil {
		return int(windows.ERROR_INVALID_PARAMETER)
	}
	parent, err := openDriverHelperParent(pid)
	if err != nil {
		return int(windows.ERROR_CANCELLED)
	}
	defer windows.CloseHandle(parent)
	event, err := windows.OpenEvent(windows.SYNCHRONIZE, false, windows.StringToUTF16Ptr(eventName))
	if err != nil {
		return int(windows.ERROR_CANCELLED)
	}
	defer windows.CloseHandle(event)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	handles := []windows.Handle{parent, event}
	status, err := windows.WaitForMultipleObjects(handles, false, 0)
	if err != nil || status != uint32(windows.WAIT_TIMEOUT) {
		return int(windows.ERROR_CANCELLED)
	}
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		for ctx.Err() == nil {
			status, err := windows.WaitForMultipleObjects(handles, false, 50)
			if err != nil || status != uint32(windows.WAIT_TIMEOUT) {
				cancel()
				return
			}
		}
	}()
	defer func() {
		cancel()
		<-monitorDone
	}()
	// Recheck immediately before the first mutation, not just in the monitor.
	status, err = windows.WaitForMultipleObjects(handles, false, 0)
	if err != nil || status != uint32(windows.WAIT_TIMEOUT) {
		return int(windows.ERROR_CANCELLED)
	}
	err = install(ctx)
	if ctx.Err() != nil {
		return int(windows.ERROR_CANCELLED)
	}
	return driverHelperExitCode(err)
}

func openDriverHelperParent(pid uint32) (windows.Handle, error) {
	parent, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) || !windows.GetCurrentProcessToken().IsElevated() {
		return parent, err
	}
	// Over-the-shoulder elevation changes users. The parent's default process
	// DACL need not admit that administrator; enable debug only while opening
	// the SYNCHRONIZE handle, then restore the helper token's previous state.
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token); err != nil {
		return 0, err
	}
	defer token.Close()
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr("SeDebugPrivilege"), &luid); err != nil {
		return 0, err
	}
	privileges := windows.Tokenprivileges{PrivilegeCount: 1}
	privileges.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
	var previous windows.Tokenprivileges
	var size uint32
	if err := windows.AdjustTokenPrivileges(token, false, &privileges, uint32(unsafe.Sizeof(previous)), &previous, &size); err != nil {
		return 0, err
	}
	parent, err = windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if restoreErr := windows.AdjustTokenPrivileges(token, false, &previous, 0, nil, nil); restoreErr != nil {
		if parent != 0 {
			windows.CloseHandle(parent)
		}
		return 0, errors.Join(err, restoreErr)
	}
	return parent, err
}

func parseDriverHelperArgs(args []string) (uint32, string, error) {
	if len(args) != 3 || args[0] != "--prepare-driver" {
		return 0, "", windows.ERROR_INVALID_PARAMETER
	}
	pid, err := strconv.ParseUint(args[1], 10, 32)
	if err != nil || pid == 0 || strconv.FormatUint(pid, 10) != args[1] || !strings.HasPrefix(args[2], driverHelperEventPrefix) {
		return 0, "", windows.ERROR_INVALID_PARAMETER
	}
	nonce := strings.TrimPrefix(args[2], driverHelperEventPrefix)
	if len(nonce) != 64 || strings.Trim(nonce, "0123456789abcdef") != "" {
		return 0, "", windows.ERROR_INVALID_PARAMETER
	}
	return uint32(pid), args[2], nil
}

func driverHelperExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), errors.Is(err, windows.ERROR_CANCELLED):
		return int(windows.ERROR_CANCELLED)
	case errors.Is(err, errDriverConflict):
		return driverHelperConflict
	case errors.Is(err, errDriverInstallInProgress):
		return driverHelperBusy
	case errors.Is(err, errDriverDisabled):
		return driverHelperDisabled
	case errors.Is(err, errDriverRemovalPending):
		return driverHelperRemoving
	case errors.Is(err, errDriverInaccessible):
		return driverHelperDenied
	default:
		var errno syscall.Errno
		if errors.As(err, &errno) && errno != 0 && errno <= driverHelperWin32Mask {
			return driverHelperWin32 | int(errno)
		}
		return driverHelperFailed
	}
}

func driverHelperResult(code uint32) error {
	switch code {
	case 0:
		return nil
	case uint32(windows.ERROR_CANCELLED):
		return windows.ERROR_CANCELLED
	case driverHelperConflict:
		return errDriverConflict
	case driverHelperBusy:
		return errDriverInstallInProgress
	case driverHelperDisabled:
		return errDriverDisabled
	case driverHelperRemoving:
		return errDriverRemovalPending
	case driverHelperDenied:
		return errDriverInaccessible
	default:
		if code & ^uint32(driverHelperWin32Mask) == driverHelperWin32 && code&driverHelperWin32Mask != 0 {
			errno := syscall.Errno(code & driverHelperWin32Mask)
			return fmt.Errorf("netfilter: driver helper failed (Win32 error %d): %w", errno, errno)
		}
		return fmt.Errorf("netfilter: driver helper failed (exit code %d)", code)
	}
}

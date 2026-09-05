//go:build windows && amd64 && game_proxy

package netfilter

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type driverState uint8

const (
	driverMissing driverState = iota
	driverStopped
	driverRunning
	driverPending
)

var (
	errDriverConflict          = errors.New("netfilter: foreign or incompatible driver installation")
	errDriverDisabled          = errors.New("netfilter: driver service is disabled")
	errDriverRemovalPending    = errors.New("netfilter: driver service is pending removal")
	errDriverInaccessible      = errors.New("netfilter: driver installation is inaccessible")
	errDriverInstallInProgress = errors.New("netfilter: driver installation is in progress or its files are busy")
)

func inspectDriver() (driverState, error) {
	manager, service, err := openDriverService(false)
	if err != nil {
		return driverMissing, err
	}
	defer windows.CloseServiceHandle(manager)
	if service != 0 {
		defer windows.CloseServiceHandle(service)
	}
	files, err := openDriverFiles()
	if err != nil {
		return driverMissing, err
	}
	defer files.close()
	if err := files.verify(service != 0); err != nil {
		return driverMissing, err
	}
	if service == 0 {
		return driverMissing, nil
	}
	return queryDriverService(service, files.sys)
}

func openDriverService(start bool) (windows.Handle, windows.Handle, error) {
	manager, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return 0, 0, driverServiceError("connect to service manager", err)
	}
	access := uint32(windows.SERVICE_QUERY_STATUS | windows.SERVICE_QUERY_CONFIG)
	if start {
		access |= windows.SERVICE_START
	}
	service, err := windows.OpenService(manager, windows.StringToUTF16Ptr(netFilterDriverName), access)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return manager, 0, nil
	}
	if err != nil {
		windows.CloseServiceHandle(manager)
		return 0, 0, driverServiceError("open driver service", err)
	}
	return manager, service, nil
}

func driverServiceError(operation string, err error) error {
	switch {
	case errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE):
		return fmt.Errorf("%w: %s: %w", errDriverRemovalPending, operation, err)
	case errors.Is(err, windows.ERROR_SERVICE_DISABLED):
		return fmt.Errorf("%w: %s: %w", errDriverDisabled, operation, err)
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return fmt.Errorf("%w: %s: %w", errDriverInaccessible, operation, err)
	default:
		return fmt.Errorf("netfilter: %s: %w", operation, err)
	}
}

func queryDriverService(service windows.Handle, sysPath string) (driverState, error) {
	// QueryServiceConfig is bounded to 8 KiB by the Windows API.
	buffer := make([]byte, 8192)
	config := (*windows.QUERY_SERVICE_CONFIG)(unsafe.Pointer(&buffer[0]))
	var needed uint32
	if err := windows.QueryServiceConfig(service, config, uint32(len(buffer)), &needed); err != nil {
		return driverMissing, driverServiceError("query driver configuration", err)
	}
	var status windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(service, &status); err != nil {
		return driverMissing, driverServiceError("query driver status", err)
	}
	state, err := classifyDriverService(config.ServiceType, config.StartType, windows.UTF16PtrToString(config.BinaryPathName), status.CurrentState, sysPath)
	runtime.KeepAlive(buffer)
	return state, err
}

func classifyDriverService(serviceType, startType uint32, imagePath string, state uint32, sysPath string) (driverState, error) {
	// The SDK stores a SystemRoot-relative kernel image, not a working-directory
	// relative path. Accept only its exact fixed name and equivalent absolute forms.
	systemImage := `System32\drivers\` + netFilterDriverName + ".sys"
	if serviceType != windows.SERVICE_KERNEL_DRIVER ||
		(!strings.EqualFold(imagePath, sysPath) &&
			!strings.EqualFold(imagePath, `\??\`+sysPath) &&
			!strings.EqualFold(imagePath, `\SystemRoot\`+systemImage) &&
			!strings.EqualFold(imagePath, systemImage)) {
		return driverMissing, fmt.Errorf("%w: service type or image path differs", errDriverConflict)
	}
	if startType == windows.SERVICE_DISABLED {
		return driverMissing, errDriverDisabled
	}
	if startType > windows.SERVICE_DEMAND_START {
		return driverMissing, fmt.Errorf("%w: unsupported service start type %d", errDriverConflict, startType)
	}
	switch state {
	case windows.SERVICE_STOPPED:
		return driverStopped, nil
	case windows.SERVICE_RUNNING:
		return driverRunning, nil
	case windows.SERVICE_START_PENDING, windows.SERVICE_STOP_PENDING, windows.SERVICE_CONTINUE_PENDING, windows.SERVICE_PAUSE_PENDING:
		return driverPending, nil
	default:
		return driverMissing, fmt.Errorf("%w: unsupported service state %d", errDriverConflict, state)
	}
}

func installDriver(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		return fmt.Errorf("netfilter: driver installation requires an elevated helper token: %w", windows.ERROR_ELEVATION_REQUIRED)
	}
	manager, service, err := openDriverService(true)
	if err != nil {
		return err
	}
	defer windows.CloseServiceHandle(manager)
	defer func() {
		if service != 0 {
			windows.CloseServiceHandle(service)
		}
	}()
	files, err := openDriverFiles()
	if err != nil {
		return err
	}
	defer files.close()
	if err := files.verify(service != 0); err != nil {
		return err
	}
	if service != 0 {
		state, err := queryDriverService(service, files.sys)
		if err != nil {
			return err
		}
		if state == driverRunning {
			return ctx.Err()
		}
		if state == driverPending {
			return fmt.Errorf("netfilter: driver service transition is pending; retry after it finishes")
		}
	}
	if !files.product {
		if err := files.createDirectory(ctx); err != nil {
			return err
		}
	}
	if err := files.lock(ctx); err != nil {
		return err
	}
	if service == 0 {
		for _, payload := range []struct {
			path     string
			contents []byte
			digest   string
		}{
			{filepath.Join(files.directory, netFilterDLLName), embeddedNetFilterDLL, netFilterDLLSHA256},
			{filepath.Join(files.directory, driverLicenseName), []byte(embeddedNetFilterLicense), netFilterLicenseSHA256},
			{files.sys, embeddedNetFilterDriver, netFilterDriverSHA256},
		} {
			if err := files.writePayload(ctx, payload.path, payload.contents, payload.digest); err != nil {
				return err
			}
		}
		// Do not let the SDK adopt an existing service. Other Bork helpers are
		// excluded by the protected lock; an uncoordinated service is a conflict.
		other, queryErr := windows.OpenService(manager, windows.StringToUTF16Ptr(netFilterDriverName), windows.SERVICE_QUERY_CONFIG|windows.SERVICE_QUERY_STATUS)
		if queryErr == nil {
			windows.CloseServiceHandle(other)
			return fmt.Errorf("%w: service appeared before registration", errDriverConflict)
		}
		if !errors.Is(queryErr, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return driverServiceError("recheck absent driver service", queryErr)
		}
		if err := files.registerDriver(ctx); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		service, err = windows.OpenService(manager, windows.StringToUTF16Ptr(netFilterDriverName), windows.SERVICE_QUERY_CONFIG|windows.SERVICE_QUERY_STATUS)
		if err != nil {
			return driverServiceError("open registered driver", err)
		}
	} else {
		// The earlier query preceded lock acquisition; only a still-verified,
		// stopped service may be started after another helper releases the lock.
		state, err := queryDriverService(service, files.sys)
		if err != nil {
			return err
		}
		if state == driverRunning {
			return ctx.Err()
		}
		if state != driverStopped {
			return errors.New("netfilter: driver service transition is pending; retry after it finishes")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := windows.StartService(service, 0, nil); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			return driverServiceError("start verified driver", err)
		}
	}
	return waitDriverRunning(ctx, service, files.sys)
}

func (files *driverFiles) registerDriver(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dllPath := filepath.Join(files.directory, netFilterDLLName)
	if files.files[dllPath] == nil || files.files[files.sys] == nil || files.files[filepath.Join(files.directory, driverMarkerName)] == nil {
		return fmt.Errorf("%w: SDK registration requires pinned owned files", errDriverConflict)
	}
	module, err := windows.LoadLibraryEx(dllPath, 0, windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
	if err != nil {
		return fmt.Errorf("load protected SDK DLL: %w", err)
	}
	defer windows.FreeLibrary(module)
	modulePath := make([]uint16, 32768)
	n, err := windows.GetModuleFileName(module, &modulePath[0], uint32(len(modulePath)))
	if err != nil {
		return fmt.Errorf("query loaded SDK DLL: %w", err)
	}
	if n >= uint32(len(modulePath)) || !strings.EqualFold(windows.UTF16ToString(modulePath[:n]), dllPath) {
		return fmt.Errorf("%w: loaded SDK module path differs", errDriverConflict)
	}
	proc, err := windows.GetProcAddress(module, "nf_registerDriver")
	if err != nil {
		return fmt.Errorf("resolve nf_registerDriver: %w", err)
	}
	name := append([]byte(netFilterDriverName), 0)
	if err := ctx.Err(); err != nil {
		return err
	}
	// Registration also starts the driver and cannot be interrupted safely.
	// SyscallN captures GetLastError on the calling thread; never fetch it later.
	runtime.LockOSThread()
	status, _, lastError := syscall.SyscallN(proc, uintptr(unsafe.Pointer(&name[0])))
	runtime.UnlockOSThread()
	runtime.KeepAlive(name)
	if int32(status) != 0 {
		return driverRegistrationError(int32(status), lastError)
	}
	return ctx.Err()
}

func driverRegistrationError(status int32, lastError syscall.Errno) error {
	if lastError == 0 {
		return fmt.Errorf("nf_registerDriver failed: NF_STATUS=%d", status)
	}
	return fmt.Errorf("nf_registerDriver failed: NF_STATUS=%d, GetLastError=%d: %w", status, uint32(lastError), lastError)
}

func waitDriverRunning(ctx context.Context, service windows.Handle, sysPath string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("wait for driver running: %w", err)
		}
		state, err := queryDriverService(service, sysPath)
		if err != nil {
			return err
		}
		switch state {
		case driverRunning:
			return nil
		case driverStopped:
			return errors.New("netfilter: driver stopped instead of reaching running state")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for driver running: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

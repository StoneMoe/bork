//go:build windows

package identity

import (
	"errors"

	"golang.org/x/sys/windows"
)

func acquirePlatformLease(peerID string) (func() error, error) {
	name, err := windows.UTF16PtrFromString(`Global\Bork.Identity.` + peerID)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return nil, ErrAlreadyActive
	}
	if err != nil {
		return nil, err
	}
	return func() error {
		return windows.CloseHandle(handle)
	}, nil
}

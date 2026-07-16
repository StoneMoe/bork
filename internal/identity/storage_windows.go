//go:build windows

package identity

import (
	"crypto/ed25519"
	"errors"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

func protectSeed(seed []byte) ([]byte, error) {
	return cryptData(seed, true)
}

func unprotectSeed(contents []byte) ([]byte, error) {
	return cryptData(contents, false)
}

func decodeStoredSeed(contents []byte) ([]byte, error) {
	if len(contents) <= len(identityMagic) || string(contents[:len(identityMagic)]) != identityMagic {
		return nil, errors.New("identity data is corrupt or unsupported")
	}
	seed, err := unprotectSeed(contents[len(identityMagic):])
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("identity data is corrupt or unavailable")
	}
	return seed, nil
}

func cryptData(input []byte, protect bool) ([]byte, error) {
	if len(input) == 0 {
		return nil, errors.New("identity data is empty")
	}
	in := windows.DataBlob{Size: uint32(len(input)), Data: &input[0]}
	var out windows.DataBlob
	var err error
	if protect {
		err = windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	} else {
		err = windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	}
	runtime.KeepAlive(input)
	if err != nil {
		return nil, err
	}
	if out.Data == nil || out.Size == 0 {
		return nil, errors.New("DPAPI returned empty identity data")
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, int(out.Size))...), nil
}

func publishFile(source, destination string) error {
	return moveFile(source, destination, windows.MOVEFILE_WRITE_THROUGH)
}

func moveFile(source, destination string, flags uint32) error {
	sourcePath, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationPath, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(sourcePath, destinationPath, flags)
}

func isPublishConflict(err error) bool {
	return errors.Is(err, windows.ERROR_ALREADY_EXISTS) ||
		errors.Is(err, windows.ERROR_FILE_EXISTS) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

func isRetryableAccessError(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

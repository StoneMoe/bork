//go:build windows

package peer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	wintunVersion       = "0.14.1"
	wintunLicenseSHA256 = "183adac21e7d96c508c8fd34d394b7b6708bc81564ad1bad61ab66143a008cd2"
	wintunLicenseSize   = 5431
	// Administrators own the protected tree; only elevated Administrators and SYSTEM get access.
	wintunRuntimeSDDL = "O:BAG:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	maxWintunFile     = 4 << 20
)

var errInvalidWintunFileSize = errors.New("Wintun file has an invalid size")

func processIsElevated() (bool, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return false, err
	}
	defer token.Close()
	return token.IsElevated(), nil
}

func ensureWintun(ctx context.Context, dll []byte, dllSize int, dllSHA256 string, license []byte) (string, []*os.File, error) {
	if err := validateEmbeddedWintun(dll, dllSize, dllSHA256, license); err != nil {
		return "", nil, err
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	directory, securityDescriptor, directoryLocks, err := wintunRuntimeDirectory()
	if err != nil {
		return "", nil, err
	}
	dllPath, err := materializeWintun(ctx, directory, securityDescriptor, dll, dllSHA256, license)
	if err != nil {
		closeRuntimeDirectoryLocks(directoryLocks)
		return "", nil, err
	}
	return dllPath, directoryLocks, nil
}

func materializeWintun(ctx context.Context, directory string, securityDescriptor *windows.SECURITY_DESCRIPTOR, dll []byte, dllSHA256 string, license []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	dllPath := filepath.Join(directory, "wintun.dll")
	licensePath := filepath.Join(directory, "LICENSE.txt")
	dllReady, err := runtimeFileMatches(dllPath, dllSHA256, securityDescriptor)
	if err != nil {
		return "", err
	}
	licenseReady, err := runtimeFileMatches(licensePath, wintunLicenseSHA256, securityDescriptor)
	if err != nil {
		return "", err
	}
	if dllReady && licenseReady {
		return dllPath, nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !dllReady {
		if err := writeRuntimeFile(dllPath, dll, dllSHA256, securityDescriptor); err != nil {
			return "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !licenseReady {
		if err := writeRuntimeFile(licensePath, license, wintunLicenseSHA256, securityDescriptor); err != nil {
			return "", err
		}
	}
	return dllPath, nil
}

func validateEmbeddedWintun(dll []byte, dllSize int, dllSHA256 string, license []byte) error {
	if len(dll) != dllSize || !matchesSHA256(dll, dllSHA256) {
		return errors.New("embedded Wintun DLL failed fixed size/SHA-256 verification")
	}
	if len(license) != wintunLicenseSize || !matchesSHA256(license, wintunLicenseSHA256) {
		return errors.New("embedded Wintun license failed fixed size/SHA-256 verification")
	}
	return nil
}

func wintunRuntimePath() (string, error) {
	programData, err := programDataPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(programData, "Bork", "Wintun-"+wintunVersion, runtime.GOARCH), nil
}

func programDataPath() (string, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	initialized := false
	err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED)
	switch {
	case err == nil:
		initialized = true
	case errors.Is(err, syscall.Errno(windows.S_FALSE)):
		initialized = true
	case errors.Is(err, syscall.Errno(windows.RPC_E_CHANGED_MODE)):
	default:
		return "", fmt.Errorf("initialize COM for Windows Known Folders: %w", err)
	}
	if initialized {
		defer windows.CoUninitialize()
	}
	path, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return "", fmt.Errorf("locate Windows ProgramData known folder: %w", err)
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("Windows ProgramData known folder path is not absolute")
	}
	return path, nil
}

func wintunRuntimeDirectory() (string, *windows.SECURITY_DESCRIPTOR, []*os.File, error) {
	programData, err := programDataPath()
	if err != nil {
		return "", nil, nil, err
	}
	if err := validateWindowsPath(programData, true); err != nil {
		return "", nil, nil, fmt.Errorf("validate Windows ProgramData known folder: %w", err)
	}
	securityDescriptor, err := windows.SecurityDescriptorFromString(wintunRuntimeSDDL)
	if err != nil {
		return "", nil, nil, fmt.Errorf("create Wintun security descriptor: %w", err)
	}
	directory := programData
	directoryLocks := make([]*os.File, 0, 3)
	for _, component := range []string{"Bork", "Wintun-" + wintunVersion, runtime.GOARCH} {
		directory = filepath.Join(directory, component)
		if err := ensureProtectedDirectory(directory, securityDescriptor); err != nil {
			closeRuntimeDirectoryLocks(directoryLocks)
			return "", nil, nil, err
		}
		lock, err := openLockedRuntimeDirectory(directory)
		if err != nil {
			closeRuntimeDirectoryLocks(directoryLocks)
			return "", nil, nil, fmt.Errorf("lock protected Wintun directory %q: %w", directory, err)
		}
		if err := verifyRuntimePathSecurity(directory, securityDescriptor); err != nil {
			_ = lock.Close()
			closeRuntimeDirectoryLocks(directoryLocks)
			return "", nil, nil, fmt.Errorf("verify locked Wintun directory %q: %w", directory, err)
		}
		directoryLocks = append(directoryLocks, lock)
	}
	return directory, securityDescriptor, directoryLocks, nil
}

func openLockedRuntimeDirectory(path string) (*os.File, error) {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(path16, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("create locked Wintun directory handle")
	}
	var handleInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &handleInfo); err != nil {
		file.Close()
		return nil, err
	}
	if handleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || handleInfo.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		file.Close()
		return nil, errors.New("locked Wintun path is not a non-reparse directory")
	}
	lockedInfo, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	currentInfo, err := os.Stat(path)
	if err != nil {
		file.Close()
		return nil, err
	}
	if !lockedInfo.IsDir() || !currentInfo.IsDir() || !os.SameFile(lockedInfo, currentInfo) {
		file.Close()
		return nil, errors.New("locked Wintun directory is not the current path")
	}
	return file, nil
}

func closeRuntimeDirectoryLocks(directoryLocks []*os.File) {
	for index := len(directoryLocks) - 1; index >= 0; index-- {
		_ = directoryLocks[index].Close()
	}
}

func ensureProtectedDirectory(path string, securityDescriptor *windows.SECURITY_DESCRIPTOR) error {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: securityDescriptor}
	err = windows.CreateDirectory(path16, &attributes)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return fmt.Errorf("create protected Wintun directory %q: %w", path, err)
	}
	if err := protectRuntimePath(path, true, securityDescriptor); err != nil {
		return fmt.Errorf("protect Wintun directory %q: %w", path, err)
	}
	return nil
}

func protectRuntimePath(path string, directory bool, securityDescriptor *windows.SECURITY_DESCRIPTOR) error {
	if err := validateWindowsPath(path, directory); err != nil {
		return err
	}
	owner, _, err := securityDescriptor.Owner()
	if err != nil {
		return err
	}
	group, _, err := securityDescriptor.Group()
	if err != nil {
		return err
	}
	dacl, _, err := securityDescriptor.DACL()
	if err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, group, dacl, nil); err != nil {
		return err
	}
	return verifyRuntimePathSecurity(path, securityDescriptor)
}

func verifyRuntimePathSecurity(path string, expected *windows.SECURITY_DESCRIPTOR) error {
	actual, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	actualOwner, actualOwnerDefaulted, err := actual.Owner()
	if err != nil {
		return err
	}
	expectedOwner, expectedOwnerDefaulted, err := expected.Owner()
	if err != nil {
		return err
	}
	actualGroup, actualGroupDefaulted, err := actual.Group()
	if err != nil {
		return err
	}
	expectedGroup, expectedGroupDefaulted, err := expected.Group()
	if err != nil {
		return err
	}
	actualDACL, actualDACLDefaulted, err := actual.DACL()
	if err != nil {
		return err
	}
	expectedDACL, expectedDACLDefaulted, err := expected.DACL()
	if err != nil {
		return err
	}
	control, _, err := actual.Control()
	if err != nil {
		return err
	}
	if !actualOwner.Equals(expectedOwner) || actualOwnerDefaulted != expectedOwnerDefaulted || !actualGroup.Equals(expectedGroup) || actualGroupDefaulted != expectedGroupDefaulted || actualDACLDefaulted != expectedDACLDefaulted || control&windows.SE_DACL_PROTECTED == 0 || !sameRuntimeACL(actualDACL, expectedDACL) {
		return fmt.Errorf("unexpected security descriptor %q", actual.String())
	}
	return nil
}

func sameRuntimeACL(actual, expected *windows.ACL) bool {
	if actual == nil || expected == nil {
		return actual == expected
	}
	if actual.AceCount != expected.AceCount {
		return false
	}
	for index := uint32(0); index < uint32(actual.AceCount); index++ {
		var actualACE, expectedACE *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(actual, index, &actualACE) != nil || windows.GetAce(expected, index, &expectedACE) != nil || actualACE.Header != expectedACE.Header || actualACE.Mask != expectedACE.Mask {
			return false
		}
		actualSID := (*windows.SID)(unsafe.Pointer(&actualACE.SidStart))
		expectedSID := (*windows.SID)(unsafe.Pointer(&expectedACE.SidStart))
		if !actualSID.Equals(expectedSID) {
			return false
		}
	}
	return true
}

func validateWindowsPath(path string, directory bool) error {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes, err := windows.GetFileAttributes(path16)
	if err != nil {
		return err
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("reparse points are not allowed")
	}
	isDirectory := attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if isDirectory != directory {
		return errors.New("path has the wrong file type")
	}
	return nil
}

func writeRuntimeFile(path string, data []byte, expectedSHA256 string, securityDescriptor *windows.SECURITY_DESCRIPTOR) error {
	if len(data) == 0 || len(data) > maxWintunFile || !matchesSHA256(data, expectedSHA256) {
		return fmt.Errorf("refuse to write %s with an invalid SHA-256", filepath.Base(path))
	}
	if matches, err := runtimeFileMatches(path, expectedSHA256, securityDescriptor); err != nil || matches {
		return err
	}
	file, temporaryPath, err := createProtectedTempFile(filepath.Dir(path), securityDescriptor)
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(temporaryPath)
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := replaceRuntimeFile(temporaryPath, path); err != nil {
		return fmt.Errorf("publish %s: %w", filepath.Base(path), err)
	}
	if err := protectRuntimePath(path, false, securityDescriptor); err != nil {
		return err
	}
	matches, err := runtimeFileMatches(path, expectedSHA256, securityDescriptor)
	if err != nil {
		return err
	}
	if !matches {
		return fmt.Errorf("published %s failed SHA-256 verification", filepath.Base(path))
	}
	return nil
}

func replaceRuntimeFile(source, destination string) error {
	source16, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destination16, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(source16, destination16, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func createProtectedTempFile(directory string, securityDescriptor *windows.SECURITY_DESCRIPTOR) (*os.File, string, error) {
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: securityDescriptor}
	for range 10 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, "", err
		}
		path := filepath.Join(directory, ".wintun-"+hex.EncodeToString(random[:])+".tmp")
		path16, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return nil, "", err
		}
		handle, err := windows.CreateFile(path16, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, &attributes, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("create protected temporary Wintun file: %w", err)
		}
		file := os.NewFile(uintptr(handle), path)
		if file == nil {
			_ = windows.CloseHandle(handle)
			return nil, "", errors.New("create protected temporary Wintun file handle")
		}
		return file, path, nil
	}
	return nil, "", errors.New("create unique protected temporary Wintun file")
}

func runtimeFileMatches(path, expectedSHA256 string, securityDescriptor *windows.SECURITY_DESCRIPTOR) (bool, error) {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err := protectRuntimePath(path, false, securityDescriptor); err != nil {
		return false, err
	}
	file, info, err := openLockedRuntimeFile(path)
	if errors.Is(err, errInvalidWintunFileSize) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	return lockedRuntimeFileMatches(file, info, expectedSHA256)
}

func openLockedRuntimeFile(path string) (*os.File, os.FileInfo, error) {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, nil, err
	}
	handle, err := windows.CreateFile(path16, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, nil, errors.New("open locked Wintun file handle")
	}
	var handleInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &handleInfo); err != nil {
		file.Close()
		return nil, nil, err
	}
	if handleInfo.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		file.Close()
		return nil, nil, errors.New("locked Wintun path is not a regular file")
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, nil, errors.New("locked Wintun path is not a regular file")
	}
	if info.Size() <= 0 || info.Size() > maxWintunFile {
		file.Close()
		return nil, nil, errInvalidWintunFileSize
	}
	return file, info, nil
}

func lockedRuntimeFileMatches(file *os.File, info os.FileInfo, expectedSHA256 string) (bool, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, maxWintunFile+1))
	if err != nil {
		return false, err
	}
	return written == info.Size() && fmt.Sprintf("%x", hash.Sum(nil)) == expectedSHA256, nil
}

func loadVerifiedWintun(path, expectedSHA256 string) (windows.Handle, error) {
	lockedFile, lockedInfo, err := openLockedRuntimeFile(path)
	if err != nil {
		return 0, fmt.Errorf("lock Wintun DLL %q: %w", path, err)
	}
	defer lockedFile.Close()
	matches, err := lockedRuntimeFileMatches(lockedFile, lockedInfo, expectedSHA256)
	if err != nil {
		return 0, fmt.Errorf("hash locked Wintun DLL %q: %w", path, err)
	}
	if !matches {
		return 0, errors.New("locked Wintun DLL has an invalid SHA-256")
	}
	module, err := windows.LoadLibraryEx(path, 0, windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
	if err != nil {
		return 0, fmt.Errorf("load verified Wintun DLL %q: %w", path, err)
	}
	loadedPath, err := loadedModulePath(module)
	if err == nil {
		var loadedInfo os.FileInfo
		loadedInfo, err = os.Stat(loadedPath)
		if err == nil && !os.SameFile(lockedInfo, loadedInfo) {
			err = fmt.Errorf("Windows loaded Wintun from %q instead of %q", loadedPath, path)
		}
	}
	if err != nil {
		_ = windows.FreeLibrary(module)
		return 0, err
	}
	return module, nil
}

func matchesSHA256(data []byte, expected string) bool {
	digest := sha256.Sum256(data)
	return len(expected) == sha256.Size*2 && fmt.Sprintf("%x", digest) == expected
}

func loadedModulePath(module windows.Handle) (string, error) {
	buffer := make([]uint16, 32768)
	length, err := windows.GetModuleFileName(module, &buffer[0], uint32(len(buffer)))
	if err != nil {
		return "", fmt.Errorf("locate loaded Wintun DLL: %w", err)
	}
	if length == 0 || length >= uint32(len(buffer)) {
		return "", errors.New("loaded Wintun DLL path is invalid")
	}
	return windows.UTF16ToString(buffer[:length]), nil
}

//go:build windows && amd64 && game_proxy

package netfilter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	driverDirectoryName = "bork-netfilter-demo"
	driverMarkerName    = "ownership"
	driverMarker        = "Bork NetFilter demo installation\nversion=1\nsdk=" + netFilterSDKVersion + "\nservice=" + netFilterDriverName + "\n"
	driverLockName      = "install.lock"
	driverLicenseName   = "license.rtf"
	driverFileSDDL      = "O:BAG:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FRFX;;;BU)"
	driverDirectorySDDL = "O:BAG:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FRFX;;;BU)"
	trustedInstallerSID = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
)

type driverFiles struct {
	directory string
	sys       string
	product   bool
	handles   []*os.File
	files     map[string]*os.File
}

func (files *driverFiles) close() {
	for i := len(files.handles) - 1; i >= 0; i-- {
		files.handles[i].Close()
	}
}

func openDriverFiles() (_ *driverFiles, err error) {
	// This package is amd64-only: GetSystemDirectory returns native System32,
	// without consulting caller-controlled environment variables or WOW64 paths.
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return nil, fmt.Errorf("locate native System32: %w", err)
	}
	volume := filepath.VolumeName(system)
	if len(volume) != 2 || volume[1] != ':' || !filepath.IsAbs(system) || filepath.Clean(system) != system {
		return nil, fmt.Errorf("%w: unexpected native System32 path", errDriverConflict)
	}
	files := &driverFiles{
		directory: filepath.Join(system, driverDirectoryName),
		sys:       filepath.Join(system, "drivers", netFilterDriverName+".sys"),
		files:     make(map[string]*os.File),
	}
	defer func() {
		if err != nil {
			files.close()
		}
	}()
	path := volume + `\`
	if err = files.pinDirectory(path, false, true); err != nil {
		return nil, err
	}
	for _, component := range strings.Split(strings.TrimPrefix(system, path), `\`) {
		path = filepath.Join(path, component)
		if err = files.pinDirectory(path, false, false); err != nil {
			return nil, err
		}
	}
	if err = files.pinDirectory(filepath.Dir(files.sys), false, false); err != nil {
		return nil, err
	}
	err = files.pinDirectory(files.directory, true, false)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return files, nil
	}
	if err != nil {
		return nil, err
	}
	files.product = true
	return files, nil
}

func (files *driverFiles) pinDirectory(path string, protected, root bool) error {
	file, err := openDriverObject(path, true, protected, root)
	if err != nil {
		return err
	}
	files.handles = append(files.handles, file)
	return nil
}

func openDriverObject(path string, directory, protected, root bool) (*os.File, error) {
	file, err := pinDriverObject(path)
	if err != nil {
		return nil, err
	}
	if err = verifyDriverObject(windows.Handle(file.Fd()), path, directory, protected, root); err != nil {
		file.Close()
		return nil, driverFileError(path, err)
	}
	return file, nil
}

func pinDriverObject(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// Every ancestor and payload stays open without write/delete sharing until
	// the privileged SDK call and service verification finish. GENERIC_READ
	// includes LIST_DIRECTORY: metadata-only opens do not enforce share denial.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, driverFileError(path, err)
	}
	return os.NewFile(uintptr(handle), path), nil
}

func verifyDriverObject(handle windows.Handle, path string, directory, protected, root bool) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		(info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory ||
		(!directory && info.NumberOfLinks != 1) {
		return fmt.Errorf("%w: reparse point, wrong object type, or multiple hard links", errDriverConflict)
	}
	buffer := make([]uint16, 32768)
	n, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
	if err != nil {
		return err
	}
	if n >= uint32(len(buffer)) || !strings.EqualFold(windows.UTF16ToString(buffer[:n]), `\\?\`+path) {
		return fmt.Errorf("%w: file does not resolve to its protected path", errDriverConflict)
	}
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return verifyDriverSecurity(sd, protected, root)
}

func verifyDriverSecurity(sd *windows.SECURITY_DESCRIPTOR, protected, root bool) error {
	if sd == nil || !sd.IsValid() {
		return fmt.Errorf("%w: invalid security descriptor", errDriverConflict)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	if owner == nil || !trustedDriverSID(owner) {
		return fmt.Errorf("%w: untrusted owner", errDriverConflict)
	}
	control, _, err := sd.Control()
	if err != nil {
		return err
	}
	if protected && control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("%w: DACL is not protected", errDriverConflict)
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		return fmt.Errorf("%w: missing DACL (%v)", errDriverConflict, err)
	}
	readOnly := windows.ACCESS_MASK(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE | windows.GENERIC_READ | windows.GENERIC_EXECUTE)
	if root {
		// Windows permits creating unrelated children at the volume root. This
		// does not permit replacing the already-pinned Windows directory.
		readOnly |= windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return fmt.Errorf("%w: unsupported access-control entry", errDriverConflict)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !trustedDriverSID(sid) && ace.Mask & ^readOnly != 0 {
			return fmt.Errorf("%w: non-administrator can modify protected object", errDriverConflict)
		}
	}
	return nil
}

func trustedDriverSID(sid *windows.SID) bool {
	return sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) || sid.IsWellKnown(windows.WinLocalSystemSid) || sid.String() == trustedInstallerSID
}

func driverFileError(path string, err error) error {
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return fmt.Errorf("%w: %s: %w", errDriverInstallInProgress, path, err)
	}
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return fmt.Errorf("%w: %s: %w", errDriverInaccessible, path, err)
	}
	return fmt.Errorf("driver file %s: %w", path, err)
}

func (files *driverFiles) verify(complete bool) error {
	if files.product {
		entries, err := os.ReadDir(files.directory)
		if err != nil {
			return driverFileError(files.directory, err)
		}
		for _, entry := range entries {
			switch entry.Name() {
			case driverMarkerName, driverLockName, netFilterDLLName, driverLicenseName:
			default:
				return fmt.Errorf("%w: unknown product-directory entry %q", errDriverConflict, entry.Name())
			}
		}
		lock, err := openDriverObject(filepath.Join(files.directory, driverLockName), false, true, false)
		if err == nil {
			err = verifyDriverContents(lock, 0, driverDigest(nil))
			lock.Close()
		}
		if err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return err
		}
		if err := files.pinPayload(filepath.Join(files.directory, driverMarkerName), []byte(driverMarker), driverDigest([]byte(driverMarker)), true); err != nil {
			return fmt.Errorf("ownership marker (installation may be incomplete or in progress): %w", err)
		}
		if err := files.pinPayload(filepath.Join(files.directory, netFilterDLLName), embeddedNetFilterDLL, netFilterDLLSHA256, complete); err != nil {
			return err
		}
		if err := files.pinPayload(filepath.Join(files.directory, driverLicenseName), []byte(embeddedNetFilterLicense), netFilterLicenseSHA256, complete); err != nil {
			return err
		}
	} else if complete {
		return fmt.Errorf("%w: service exists without Bork ownership directory", errDriverConflict)
	}
	if err := files.pinPayload(files.sys, embeddedNetFilterDriver, netFilterDriverSHA256, complete); err != nil {
		return err
	}
	if !files.product && files.files[files.sys] != nil {
		return fmt.Errorf("%w: orphan SYS without Bork ownership", errDriverConflict)
	}
	return nil
}

func (files *driverFiles) pinPayload(path string, contents []byte, digest string, required bool) error {
	file, err := openDriverObject(path, false, true, false)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		if required {
			return fmt.Errorf("%w: required file missing: %s", errDriverConflict, path)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if err := verifyDriverContents(file, int64(len(contents)), digest); err != nil {
		file.Close()
		return driverFileError(path, err)
	}
	files.handles = append(files.handles, file)
	files.files[path] = file
	return nil
}

func driverDigest(contents []byte) string {
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}

func verifyDriverContents(file *os.File, size int64, digest string) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != size {
		return fmt.Errorf("%w: unexpected file size", errDriverConflict)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, size+1)); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return fmt.Errorf("%w: pinned SHA-256 mismatch", errDriverConflict)
	}
	return nil
}

func driverSecurityAttributes(directory bool) (*windows.SecurityAttributes, error) {
	sddl := driverFileSDDL
	if directory {
		sddl = driverDirectorySDDL
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}, nil
}

func (files *driverFiles) createDirectory(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	attributes, err := driverSecurityAttributes(true)
	if err != nil {
		return err
	}
	err = windows.CreateDirectory(windows.StringToUTF16Ptr(files.directory), attributes)
	runtime.KeepAlive(attributes)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return fmt.Errorf("%w: product directory appeared concurrently", errDriverInstallInProgress)
	}
	if err != nil {
		return driverFileError(files.directory, err)
	}
	if err := files.pinDirectory(files.directory, true, false); err != nil {
		return err
	}
	files.product = true
	// Once created, finish the ownership bootstrap before honoring cancellation;
	// otherwise an ordinary cancellation would strand an unowned directory.
	if err := files.writePayload(context.WithoutCancel(ctx), filepath.Join(files.directory, driverMarkerName), []byte(driverMarker), driverDigest([]byte(driverMarker))); err != nil {
		return err
	}
	return ctx.Err()
}

func (files *driverFiles) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	attributes, err := driverSecurityAttributes(false)
	if err != nil {
		return err
	}
	path := filepath.Join(files.directory, driverLockName)
	handle, err := windows.CreateFile(windows.StringToUTF16Ptr(path), windows.GENERIC_READ|windows.READ_CONTROL, 0,
		attributes, windows.OPEN_ALWAYS, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	runtime.KeepAlive(attributes)
	if err != nil {
		return driverFileError(path, err)
	}
	file := os.NewFile(uintptr(handle), path)
	if err = verifyDriverObject(handle, path, false, true, false); err == nil {
		err = verifyDriverContents(file, 0, driverDigest(nil))
	}
	if err != nil {
		file.Close()
		return driverFileError(path, err)
	}
	files.handles = append(files.handles, file)
	return nil
}

func (files *driverFiles) writePayload(ctx context.Context, path string, contents []byte, digest string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if files.files[path] != nil {
		return nil
	}
	if driverDigest(contents) != digest {
		return fmt.Errorf("%w: embedded payload SHA-256 mismatch", errDriverConflict)
	}
	attributes, err := driverSecurityAttributes(false)
	if err != nil {
		return err
	}
	// CREATE_NEW never truncates/adopts an unknown object. Parents are pinned;
	// the explicit protected DACL excludes untrusted writers during creation.
	handle, err := windows.CreateFile(windows.StringToUTF16Ptr(path), windows.GENERIC_WRITE|windows.DELETE, 0,
		attributes, windows.CREATE_NEW, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	runtime.KeepAlive(attributes)
	if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return fmt.Errorf("%w: file appeared concurrently: %s", errDriverConflict, path)
	}
	if err != nil {
		return driverFileError(path, err)
	}
	file := os.NewFile(uintptr(handle), path)
	_, err = file.Write(contents)
	if err == nil {
		err = file.Sync()
	}
	if err != nil {
		// Delete only the new file through its own handle, never by pathname.
		remove := byte(1)
		cleanupErr := windows.SetFileInformationByHandle(handle, windows.FileDispositionInfo, &remove, 1)
		file.Close()
		return driverFileError(path, errors.Join(err, cleanupErr))
	}
	if err := file.Close(); err != nil {
		return driverFileError(path, err)
	}
	return files.pinPayload(path, contents, digest, true)
}

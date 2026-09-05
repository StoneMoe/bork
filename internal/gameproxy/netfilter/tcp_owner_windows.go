//go:build windows

package netfilter

import (
	"net/netip"
	"syscall"
	"unsafe"

	"bork/internal/gameproxy/intercept"
	"golang.org/x/sys/windows"
)

var getExtendedTCPTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

func tcpSocketOwner(local, remote netip.AddrPort) (intercept.ProcessID, error) {
	if err := getExtendedTCPTable.Find(); err != nil {
		return 0, err
	}
	const tcpTableOwnerPIDAll = 5
	buffer := make([]byte, 4)
	// Allow bounded buffer growth while connections change; do not poll for
	// a missing tuple or reuse cached ownership from an earlier connection.
	for range 3 {
		size := uint32(len(buffer))
		status, _, _ := getExtendedTCPTable.Call(
			uintptr(unsafe.Pointer(&buffer[0])), uintptr(unsafe.Pointer(&size)),
			0, windows.AF_INET, tcpTableOwnerPIDAll, 0,
		)
		if status == 0 {
			return tcpOwnerFromTable(buffer, local, remote)
		}
		if status != uintptr(windows.ERROR_INSUFFICIENT_BUFFER) {
			return 0, syscall.Errno(status)
		}
		if size <= uint32(len(buffer)) {
			return 0, ErrTCPRedirectOwner
		}
		buffer = make([]byte, size)
	}
	return 0, windows.ERROR_INSUFFICIENT_BUFFER
}

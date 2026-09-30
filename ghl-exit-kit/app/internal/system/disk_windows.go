//go:build windows

package system

import (
	"syscall"
	"unsafe"
)

var procGetDiskFreeSpaceExW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// diskFree returns the bytes available to the current user on the volume
// holding path (GetDiskFreeSpaceExW).
func diskFree(path string) (uint64, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	r, _, callErr := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&free)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r == 0 {
		return 0, callErr
	}
	return free, nil
}

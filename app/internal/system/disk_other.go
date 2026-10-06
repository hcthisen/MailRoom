//go:build !linux && !darwin && !freebsd && !windows

package system

// diskFree is not implemented on this platform; the disk_space check is
// skipped.
func diskFree(string) (uint64, error) { return 0, errDiskUnsupported }

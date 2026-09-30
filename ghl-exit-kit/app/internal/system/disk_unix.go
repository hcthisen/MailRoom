//go:build linux || darwin || freebsd

package system

import "syscall"

// diskFree returns the bytes available to unprivileged users on the file
// system holding path.
func diskFree(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

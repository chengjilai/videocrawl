//go:build linux

package app

import "syscall"

// diskFreeBytes returns the free bytes on the filesystem containing path
// (the df numbers: bavail = blocks available to unprivileged users).
func diskFreeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

//go:build !windows

package server

import "golang.org/x/sys/unix"

func diskKOctets(path string) (int64, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, false
	}
	size := int64(st.Bsize) * int64(st.Blocks)
	if size <= 0 {
		return 0, false
	}
	return size / 1024, true
}

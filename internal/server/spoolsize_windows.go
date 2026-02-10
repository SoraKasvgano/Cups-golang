//go:build windows

package server

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

func diskKOctets(path string) (int64, bool) {
	clean := filepath.Clean(path)
	if clean == "" || clean == "." {
		return 0, false
	}
	ptr, err := windows.UTF16PtrFromString(clean)
	if err != nil {
		return 0, false
	}
	var free, total, freeTotal uint64
	if err := windows.GetDiskFreeSpaceEx(ptr, &free, &total, &freeTotal); err != nil {
		return 0, false
	}
	if total == 0 {
		return 0, false
	}
	return int64(total / 1024), true
}

//go:build linux

package app

import (
	"golang.org/x/sys/unix"
	"os"
)

func creationTime(path string, _ os.FileInfo) int64 {
	var stat unix.Statx_t
	if unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &stat) == nil && stat.Mask&unix.STATX_BTIME != 0 {
		return stat.Btime.Sec*1000 + int64(stat.Btime.Nsec)/1e6
	}
	return 0
}

//go:build windows

package app

import (
	"os"
	"syscall"
)

func creationTime(_ string, info os.FileInfo) int64 {
	if data, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		return data.CreationTime.Nanoseconds() / 1e6
	}
	return 0
}

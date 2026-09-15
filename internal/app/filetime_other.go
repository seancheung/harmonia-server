//go:build !windows && !linux

package app

import "os"

func creationTime(_ string, _ os.FileInfo) int64 { return 0 }

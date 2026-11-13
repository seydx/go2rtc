//go:build unix

package app

import (
	"os"
	"syscall"
)

func keepOwner(f *os.File, old os.FileInfo) error {
	stat, ok := old.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return f.Chown(int(stat.Uid), int(stat.Gid))
}

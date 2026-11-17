//go:build !windows

package scanner

import (
	"os"
	"syscall"
)

type FileStat struct {
	Device uint64
	Inode  uint64
	Nlink  int
}

func StatPath(_ string, info os.FileInfo) (FileStat, error) {
	if info == nil {
		return FileStat{}, nil
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return FileStat{}, nil
	}
	return FileStat{
		Device: uint64(st.Dev),
		Inode:  uint64(st.Ino),
		Nlink:  int(st.Nlink),
	}, nil
}

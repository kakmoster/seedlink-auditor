//go:build windows

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

func StatPath(path string, _ os.FileInfo) (FileStat, error) {
	pPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return FileStat{}, err
	}
	handle, err := syscall.CreateFile(
		pPtr,
		0,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return FileStat{}, err
	}
	defer syscall.CloseHandle(handle)

	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(handle, &info); err != nil {
		return FileStat{}, err
	}
	return FileStat{
		Device: uint64(info.VolumeSerialNumber),
		Inode:  (uint64(info.FileIndexHigh) << 32) | uint64(info.FileIndexLow),
		Nlink:  int(info.NumberOfLinks),
	}, nil
}

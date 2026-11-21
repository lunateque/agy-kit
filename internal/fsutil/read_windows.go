//go:build windows

package fsutil

import (
	"io"
	"os"
	"syscall"
)

// ReadFileShared opens a file with FILE_SHARE_READ|FILE_SHARE_WRITE|FILE_SHARE_DELETE
// so concurrent atomic renames on Windows never fail with ERROR_SHARING_VIOLATION.
func ReadFileShared(path string) ([]byte, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(
		p,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(h), path)
	defer f.Close()
	return io.ReadAll(f)
}

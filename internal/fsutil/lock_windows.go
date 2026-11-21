//go:build windows

package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

const (
	lockfileFailImmediately = 0x00000001
	lockfileExclusiveLock   = 0x00000002

	errSharingViolation = syscall.Errno(32)
	errLockViolation    = syscall.Errno(33)
)

var (
	kernel32       = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx = kernel32.NewProc("LockFileEx")
	procUnlockFile = kernel32.NewProc("UnlockFileEx")
)

// ErrLocked indicates another live process currently holds the kernel file lock.
var ErrLocked = errors.New("file lock already held")

// FileLock holds an OS kernel file lock that auto-releases if the process crashes.
type FileLock struct {
	f *os.File
}

// TryLockFile attempts a non-blocking exclusive LockFileEx lock on lockPath.
// Returns ErrLocked only when another live process holds the lock; surfaces real
// OS errors (permissions, invalid path, etc.) to the caller.
func TryLockFile(lockPath string) (*FileLock, error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	var ov syscall.Overlapped
	r, _, callErr := procLockFileEx.Call(
		f.Fd(),
		uintptr(lockfileExclusiveLock|lockfileFailImmediately),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&ov)),
	)
	if r == 0 {
		_ = f.Close()
		if callErr == errLockViolation || callErr == errSharingViolation {
			return nil, ErrLocked
		}
		return nil, callErr
	}
	return &FileLock{f: f}, nil
}

// Unlock releases the kernel lock and closes the file handle.
func (l *FileLock) Unlock() {
	if l == nil || l.f == nil {
		return
	}
	var ov syscall.Overlapped
	_, _, _ = procUnlockFile.Call(l.f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&ov)))
	_ = l.f.Close()
	l.f = nil
}

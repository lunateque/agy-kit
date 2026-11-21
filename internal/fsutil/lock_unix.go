//go:build !windows

package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLocked indicates another live process currently holds the kernel file lock.
var ErrLocked = errors.New("file lock already held")

// FileLock holds an OS kernel flock that auto-releases if the process crashes.
type FileLock struct {
	f *os.File
}

// TryLockFile attempts a non-blocking exclusive flock on lockPath.
func TryLockFile(lockPath string) (*FileLock, error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return &FileLock{f: f}, nil
}

// Unlock releases the flock and closes the file descriptor.
func (l *FileLock) Unlock() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}

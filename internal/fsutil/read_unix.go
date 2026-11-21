//go:build !windows

package fsutil

import "os"

// ReadFileShared reads path using standard POSIX open (renames are always safe).
func ReadFileShared(path string) ([]byte, error) {
	return os.ReadFile(path)
}

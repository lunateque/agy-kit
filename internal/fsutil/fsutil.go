package fsutil

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SafeID sanitizes a conversation or session ID for safe filesystem paths.
func SafeID(id string) string {
	if id == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String()
}

// ValidateStrictID verifies an ID contains only [A-Za-z0-9_-] and is non-empty,
// preventing path traversal when used in CLI flags like --conv.
func ValidateStrictID(id string) error {
	if id == "" || len(id) > 128 {
		return errors.New("invalid conversation id length")
	}
	for _, r := range id {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return errors.New("conversation id contains invalid characters")
		}
	}
	return nil
}

// ValidateSafeArg checks that a CLI flag value (model, session_id, thinking)
// contains only safe characters and cannot inject shell metacharacters.
func ValidateSafeArg(val string) error {
	if val == "" {
		return nil
	}
	if len(val) > 128 {
		return errors.New("argument exceeds 128 characters")
	}
	for _, r := range val {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == ':' || r == '/' || r == '@' || r == '+' || r == '-') {
			return errors.New("argument contains disallowed characters")
		}
	}
	return nil
}

// WriteBytesAtomic writes data via a temp file in the target directory and
// renames it into place with Windows-friendly retry backoff.
func WriteBytesAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".agy-tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	var renameErr error
	for attempt := 0; attempt < 8; attempt++ {
		if renameErr = os.Rename(tmp, path); renameErr == nil {
			return nil
		}
		time.Sleep(time.Duration(5*(attempt+1)) * time.Millisecond)
	}
	_ = os.Remove(tmp)
	return renameErr
}

// SaveJSONAtomic marshals v as JSON and writes it atomically to path.
func SaveJSONAtomic(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteBytesAtomic(path, data)
}

// SaveJSONIndentAtomic marshals v as pretty JSON with trailing newline atomically.
func SaveJSONIndentAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return WriteBytesAtomic(path, data)
}

// WithKernelLock runs fn ONLY if it successfully acquires the OS-level exclusive
// lock within waitTimeout. Returns false without calling fn if the lock times out
// or fails, guaranteeing no unlocked concurrent critical section ever runs.
func WithKernelLock(lockPath string, waitTimeout time.Duration, fn func()) bool {
	deadline := time.Now().Add(waitTimeout)
	var lk *FileLock
	for {
		var err error
		lk, err = TryLockFile(lockPath)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrLocked) {
			return false
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer lk.Unlock()
	fn()
	return true
}

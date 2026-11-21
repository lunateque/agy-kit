package fsutil

import (
	"path/filepath"
	"testing"
)

func TestValidateStrictID(t *testing.T) {
	valid := []string{"abc", "conv-123_XYZ", "7118de27-628a-4069-b388-c856a6749fe6"}
	for _, id := range valid {
		if err := ValidateStrictID(id); err != nil {
			t.Fatalf("expected valid id %q, got err: %v", id, err)
		}
	}
	invalid := []string{"", "../evil", "a/b", "a\\b", "conv id", "conv.json"}
	for _, id := range invalid {
		if err := ValidateStrictID(id); err == nil {
			t.Fatalf("expected invalid id %q to fail", id)
		}
	}
}

func TestKernelLockMutualExclusionAndRelease(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "worker.lock")
	lk1, err := TryLockFile(lockPath)
	if err != nil {
		t.Fatalf("first TryLockFile failed: %v", err)
	}
	if _, err := TryLockFile(lockPath); err == nil {
		lk1.Unlock()
		t.Fatal("expected second TryLockFile to fail while lock held")
	}
	lk1.Unlock()

	lk2, err := TryLockFile(lockPath)
	if err != nil {
		t.Fatalf("TryLockFile after Unlock failed: %v", err)
	}
	lk2.Unlock()
}

func TestWriteAndReadSharedAtomic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "data.json")
	if err := WriteBytesAtomic(p, []byte(`{"v":1}`)); err != nil {
		t.Fatalf("WriteBytesAtomic failed: %v", err)
	}
	got, err := ReadFileShared(p)
	if err != nil || string(got) != `{"v":1}` {
		t.Fatalf("ReadFileShared = %q, err=%v", string(got), err)
	}
}

package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLockDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	release, err := LockDataDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, LockFileName))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("lock file: %v %v", fi, err)
	}
	if _, err := LockDataDir(dir); !errors.Is(err, ErrDataDirLocked) {
		t.Fatalf("second lock: got %v, want ErrDataDirLocked", err)
	}
	release()
	release() // idempotent
	release2, err := LockDataDir(dir)
	if err != nil {
		t.Fatalf("relock after release: %v", err)
	}
	release2()
}

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLockInstanceRejectsSecondHolder(t *testing.T) {
	db := filepath.Join(t.TempDir(), "nested", "dispatch.db")

	release, err := lockInstance(db)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}

	if _, err := lockInstance(db); err == nil {
		t.Fatal("second lock succeeded; want an already-running error")
	} else if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second lock error = %v, want an already-running error", err)
	}

	pid, err := os.ReadFile(db + ".lock")
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	if strings.TrimSpace(string(pid)) != strconv.Itoa(os.Getpid()) {
		t.Errorf("lock file holds %q, want pid %d", strings.TrimSpace(string(pid)), os.Getpid())
	}

	release()

	release2, err := lockInstance(db)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	release2()
}

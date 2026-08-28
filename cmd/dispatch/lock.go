package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// lockInstance takes an exclusive advisory lock beside the database so only
// one dispatch serves a given store. Two instances sharing a config both
// connect to Slack, and Socket Mode hands each event to only one of them —
// the other has never seen the thread and drops the message silently, which
// looks like the bot going deaf mid-conversation.
//
// The returned release function unlocks and closes the file.
func lockInstance(dbPath string) (func(), error) {
	path := dbPath + ".lock"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder, _ := io.ReadAll(f) // best effort: the holder writes its pid
		f.Close()
		if pid := strings.TrimSpace(string(holder)); pid != "" {
			return nil, fmt.Errorf("another dispatch is already running (pid %s) on %s — stop it first", pid, dbPath)
		}
		return nil, fmt.Errorf("another dispatch is already running on %s — stop it first", dbPath)
	}
	if err := f.Truncate(0); err == nil {
		if _, err := f.Seek(0, io.SeekStart); err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
		}
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

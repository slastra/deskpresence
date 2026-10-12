package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestReadInput pins that a level counts only while hypridle runs: a stale
// "active" left by a crashed hypridle must not keep the panel lit.
func TestReadInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input")
	if readInput(path, true) != InputUnknown {
		t.Error("a missing file is unknown")
	}
	os.WriteFile(path, []byte("away 1790500000\n"), 0o644)
	if readInput(path, true) != InputAway {
		t.Error("away should read as away")
	}
	if readInput(path, false) != InputUnknown {
		t.Error("with hypridle down every level is unknown")
	}
	os.WriteFile(path, []byte("sleepy 1\n"), 0o644)
	if readInput(path, true) != InputUnknown {
		t.Error("a bad level is unknown")
	}
}

// A write by `deskpresence input` wakes the loop; other files in the
// directory do not.
func TestWatchInput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deskpresence.input")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := watchInput(ctx, path)
	if ch == nil {
		t.Skip("no inotify here")
	}
	if err := os.WriteFile(filepath.Join(dir, "other"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
		t.Fatal("woke for another file")
	case <-time.After(100 * time.Millisecond):
	}
	runInput([]string{"active"}, path)
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("no wake after an input write")
	}
	if got := readInput(path, true); got != InputActive {
		t.Fatalf("readInput = %q, want active", got)
	}
}

package main

import (
	"os"
	"path/filepath"
	"testing"
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

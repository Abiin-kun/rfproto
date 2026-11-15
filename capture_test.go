package main

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// TestSeedSessions pins the fix for a real evidence-integrity bug: the session
// counter was per-process, so restarting `serve` reused capture/c1 and merged a
// new run's frames into the previous run's directory (observed 2026-10-17: the
// 15:28 session and the 15:01 session share c1, and both a 143B and a 144B
// frame are named 004).
func TestSeedSessions(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"c1", "c3", "c2"} {
		if err := os.Mkdir(filepath.Join(dir, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Decoys that must not be treated as sessions.
	if err := os.WriteFile(filepath.Join(dir, "c9.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}

	prev := atomic.LoadInt64(&wsSessions)
	defer atomic.StoreInt64(&wsSessions, prev)

	atomic.StoreInt64(&wsSessions, 0)
	seedSessions(dir)
	if got := atomic.LoadInt64(&wsSessions); got != 3 {
		t.Fatalf("seeded counter = %d, want 3", got)
	}
	if got := atomic.AddInt64(&wsSessions, 1); got != 4 {
		t.Fatalf("next session = c%d, want c4", got)
	}

	// A missing directory must be a no-op, not a panic.
	atomic.StoreInt64(&wsSessions, 0)
	seedSessions(filepath.Join(dir, "does-not-exist"))
	if got := atomic.LoadInt64(&wsSessions); got != 0 {
		t.Fatalf("empty dir seeded to %d, want 0", got)
	}
}

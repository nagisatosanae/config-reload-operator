package main

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testRetryMin = 20 * time.Millisecond
	testRetryMax = 50 * time.Millisecond
)

// startKeepWatching runs keepWatching in the background, with short waits
// between tries, and returns a counter of reloads.
func startKeepWatching(t *testing.T, dir string, check func() error) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	keep := &memory{started: time.Now(), running: hashDir(dir)}
	go keepWatching(keep, dir, testQuiet, 0, check, func() { n.Add(1) }, testRetryMin, testRetryMax)
	time.Sleep(50 * time.Millisecond)
	return &n
}

func TestKeepWatchingStartsWhenTheDirectoryAppears(t *testing.T) {
	// The directory is not there yet, so watching cannot start.
	dir := filepath.Join(t.TempDir(), "conf")
	n := startKeepWatching(t, dir, func() error { return nil })

	// It appears, with a config in it. That is a change compared to what the
	// app started with, and it was made while nobody was watching.
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, confName)
	if err := os.WriteFile(file, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(settle)
	if got := n.Load(); got != 1 {
		t.Fatalf("reloads after the directory appeared = %d, want 1", got)
	}

	// From here on it is watched like any other directory.
	if err := os.WriteFile(file, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(settle)
	if got := n.Load(); got != 2 {
		t.Fatalf("reloads after one more change = %d, want 2", got)
	}
}

func TestKeepWatchingSurvivesAPanic(t *testing.T) {
	v := newKubeletVolume(t, conf("v1"))

	// The first check hits a bug. Every later one is fine.
	var calls atomic.Int32
	check := func() error {
		if calls.Add(1) == 1 {
			panic("a bug in the watcher")
		}
		return nil
	}
	n := startKeepWatching(t, v.dir, check)

	// The update that hit the bug is not lost: watching starts again, sees
	// that the content is not what the app runs, and reloads.
	swap(t, v, "..2026_01_01_00_00_05.2", conf("v2"))
	time.Sleep(settle)
	if got := n.Load(); got != 1 {
		t.Fatalf("reloads after a panic = %d, want 1", got)
	}

	swap(t, v, "..2026_01_01_00_00_10.3", conf("v3"))
	time.Sleep(settle)
	if got := n.Load(); got != 2 {
		t.Fatalf("reloads after one more update = %d, want 2", got)
	}
}

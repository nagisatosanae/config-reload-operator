package main

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testQuiet = 80 * time.Millisecond
	settle    = 400 * time.Millisecond
)

// conf returns the files of a volume with one config file.
func conf(content string) map[string]string {
	return map[string]string{confName: content}
}

// startWatch runs watch in the background and returns a counter of reloads.
func startWatch(t *testing.T, dir string, grace time.Duration) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	go watch(dir, testQuiet, grace, func() error { return nil }, func() { n.Add(1) })
	time.Sleep(50 * time.Millisecond) // let the watcher add the directory
	return &n
}

func swap(t *testing.T, v *kubeletVolume, stamp string, files map[string]string) {
	t.Helper()
	old := v.cur
	v.writeStampDir(stamp, files)
	v.must(os.Chmod(filepath.Join(v.dir, stamp), 0o755))
	v.must(os.Symlink(stamp, filepath.Join(v.dir, "..data_tmp")))
	v.must(os.Rename(filepath.Join(v.dir, "..data_tmp"), filepath.Join(v.dir, "..data")))
	v.must(os.RemoveAll(filepath.Join(v.dir, old)))
	v.cur = stamp
}

func TestWatchKubeletUpdateReloadsOnce(t *testing.T) {
	v := newKubeletVolume(t, conf("v1"))
	n := startWatch(t, v.dir, 0)

	swap(t, v, "..2026_01_01_00_00_05.2", conf("v2"))
	time.Sleep(settle)
	if got := n.Load(); got != 1 {
		t.Fatalf("reloads after one update = %d, want 1", got)
	}
}

func TestWatchSameContentDoesNotReload(t *testing.T) {
	v := newKubeletVolume(t, conf("v1"))
	n := startWatch(t, v.dir, 0)

	swap(t, v, "..2026_01_01_00_00_05.2", conf("v1"))
	time.Sleep(settle)
	if got := n.Load(); got != 0 {
		t.Fatalf("reloads after same-content update = %d, want 0", got)
	}
}

func TestWatchBurstOfUpdatesReloadsOnce(t *testing.T) {
	v := newKubeletVolume(t, conf("v1"))
	n := startWatch(t, v.dir, 0)

	swap(t, v, "..2026_01_01_00_00_05.2", conf("v2"))
	time.Sleep(testQuiet / 4)
	swap(t, v, "..2026_01_01_00_00_06.3", conf("v3"))
	time.Sleep(testQuiet / 4)
	swap(t, v, "..2026_01_01_00_00_07.4", conf("v4"))
	time.Sleep(settle)
	if got := n.Load(); got != 1 {
		t.Fatalf("reloads after three quick updates = %d, want 1", got)
	}
}

func TestWatchPlainFileEditReloadsOnce(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.conf")
	if err := os.WriteFile(p, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	n := startWatch(t, dir, 0)

	// truncate, then write: two steps, like an editor or "echo > file"
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(settle)
	if got := n.Load(); got != 1 {
		t.Fatalf("reloads after an in-place edit = %d, want 1", got)
	}
}

func TestWatchHoldsBackDuringStartupGrace(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.conf")
	if err := os.WriteFile(p, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	grace := 600 * time.Millisecond
	n := startWatch(t, dir, grace)

	// a change right after start, like a start script that edits the config
	if err := os.WriteFile(p, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(testQuiet + 150*time.Millisecond)
	if got := n.Load(); got != 0 {
		t.Fatalf("reloads during grace = %d, want 0", got)
	}

	// after the grace time the change is not lost: it is delivered once
	time.Sleep(grace)
	if got := n.Load(); got != 1 {
		t.Fatalf("reloads after grace = %d, want 1", got)
	}
}

func TestWatchKeepsWorkingAfterAnUpdate(t *testing.T) {
	v := newKubeletVolume(t, conf("v1"))
	n := startWatch(t, v.dir, 0)

	swap(t, v, "..2026_01_01_00_00_05.2", conf("v2"))
	time.Sleep(settle)
	swap(t, v, "..2026_01_01_00_00_09.3", conf("v3"))
	time.Sleep(settle)
	if got := n.Load(); got != 2 {
		t.Fatalf("reloads after two separate updates = %d, want 2", got)
	}
}

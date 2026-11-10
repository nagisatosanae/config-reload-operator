package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	// firstStamp is the timestamp directory a volume starts with. The
	// kubelet names these directories after the time of the update.
	firstStamp = "..2026_01_01_00_00_00.1"
	confName   = "default.conf"
)

// kubeletVolume lays files out the way the kubelet does for a ConfigMap
// volume, and can swap in a new version step by step.
type kubeletVolume struct {
	t   *testing.T
	dir string
	cur string // current timestamp directory name
}

func newKubeletVolume(t *testing.T, files map[string]string) *kubeletVolume {
	t.Helper()
	v := &kubeletVolume{t: t, dir: t.TempDir()}
	v.writeStampDir(firstStamp, files)
	v.must(os.Symlink(firstStamp, filepath.Join(v.dir, "..data")))
	v.cur = firstStamp
	// user-visible symlinks: one per top-level path element
	seen := map[string]bool{}
	for name := range files {
		top, _, _ := strings.Cut(name, "/")
		if seen[top] {
			continue
		}
		seen[top] = true
		v.must(os.Symlink(filepath.Join("..data", top), filepath.Join(v.dir, top)))
	}
	return v
}

func (v *kubeletVolume) must(err error) {
	v.t.Helper()
	if err != nil {
		v.t.Fatal(err)
	}
}

func (v *kubeletVolume) writeStampDir(stamp string, files map[string]string) {
	v.t.Helper()
	for name, content := range files {
		p := filepath.Join(v.dir, stamp, name)
		v.must(os.MkdirAll(filepath.Dir(p), 0o755))
		v.must(os.WriteFile(p, []byte(content), 0o644))
	}
}

func TestHashDirFollowsKubeletSwap(t *testing.T) {
	version := func(v string) map[string]string {
		return map[string]string{confName: "return 200 " + v + ";", "extra.conf": "# extra"}
	}
	v := newKubeletVolume(t, version("v1"))
	h1 := hashDir(v.dir)

	// step 1+2: new timestamp dir with new content (events CREATE, ATTRIB)
	v.writeStampDir("..2026_01_01_00_00_05.2", version("v2"))
	if got := hashDir(v.dir); got != h1 {
		t.Fatal("hash changed when only the new timestamp dir was written")
	}

	// step 3: ..data_tmp symlink (event CREATE)
	v.must(os.Symlink("..2026_01_01_00_00_05.2", filepath.Join(v.dir, "..data_tmp")))
	if got := hashDir(v.dir); got != h1 {
		t.Fatal("hash changed when only ..data_tmp was created")
	}

	// step 4+5: rename ..data_tmp -> ..data (events MOVED_FROM, MOVED_TO)
	v.must(os.Rename(filepath.Join(v.dir, "..data_tmp"), filepath.Join(v.dir, "..data")))
	h2 := hashDir(v.dir)
	if h2 == h1 {
		t.Fatal("hash did not change after ..data was swapped")
	}

	// step 6: old timestamp dir removed (event DELETE)
	v.must(os.RemoveAll(filepath.Join(v.dir, firstStamp)))
	if got := hashDir(v.dir); got != h2 {
		t.Fatal("hash changed when only the old timestamp dir was removed")
	}

	// same content written again as a new version: no change
	v.writeStampDir("..2026_01_01_00_00_09.3", version("v2"))
	v.must(os.Symlink("..2026_01_01_00_00_09.3", filepath.Join(v.dir, "..data_tmp")))
	v.must(os.Rename(filepath.Join(v.dir, "..data_tmp"), filepath.Join(v.dir, "..data")))
	if got := hashDir(v.dir); got != h2 {
		t.Fatal("hash changed although the content is the same")
	}
}

func TestHashDirSeesNestedItems(t *testing.T) {
	v := newKubeletVolume(t, map[string]string{
		"nginx.conf":       "main",
		"sites/a.conf":     "site a v1",
		"sites/deep/b.txt": "b",
	})
	h1 := hashDir(v.dir)

	v.writeStampDir("..2026_01_01_00_00_05.2", map[string]string{
		"nginx.conf":       "main",
		"sites/a.conf":     "site a v2",
		"sites/deep/b.txt": "b",
	})
	v.must(os.Symlink("..2026_01_01_00_00_05.2", filepath.Join(v.dir, "..data_tmp")))
	v.must(os.Rename(filepath.Join(v.dir, "..data_tmp"), filepath.Join(v.dir, "..data")))
	if hashDir(v.dir) == h1 {
		t.Fatal("a change in a nested file was not seen")
	}
}

func TestHashDirPlainDirectory(t *testing.T) {
	dir := t.TempDir()
	w := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w("a.conf", "one")
	w("b.conf", "two")
	h1 := hashDir(dir)
	if hashDir(dir) != h1 {
		t.Fatal("hash is not stable")
	}

	w("a.conf", "ONE")
	h2 := hashDir(dir)
	if h2 == h1 {
		t.Fatal("content change not seen")
	}

	if err := os.Rename(filepath.Join(dir, "b.conf"), filepath.Join(dir, "c.conf")); err != nil {
		t.Fatal(err)
	}
	h3 := hashDir(dir)
	if h3 == h2 {
		t.Fatal("rename not seen")
	}

	// "ab" + "c" must not hash the same as "a" + "bc"
	d1, d2 := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(d1, "ab"), []byte("c"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d2, "a"), []byte("bc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hashDir(d1) == hashDir(d2) {
		t.Fatal("name and content run together")
	}
}

func TestHashDirMissingAndLoop(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	first := hashDir(missing)
	if hashDir(missing) != first {
		t.Fatal("hash of a missing dir is not stable")
	}

	dir := t.TempDir()
	if err := os.Symlink(dir, filepath.Join(dir, "loop")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = hashDir(dir) // must return, not hang
}

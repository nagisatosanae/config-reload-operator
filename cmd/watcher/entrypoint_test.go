package main

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests run the real start script, watcher-entrypoint.sh, with the
// test binary standing in for the watcher. See TestMain.

// runMainEnv turns the test binary into the watcher itself.
const runMainEnv = "WATCHER_TEST_RUN_MAIN"

// TestMain lets the test binary play the watcher. When runMainEnv is set, it
// runs no tests: it calls main, so the binary does what the real watcher
// does with the same arguments.
func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// image is a directory laid out like the app image: the start script, and
// next to it the watcher.
type image struct {
	script  string
	watcher string
}

// newImage copies the start script into a new directory. With withWatcher,
// the watcher is put next to it, as a link to the test binary.
func newImage(t *testing.T, withWatcher bool) image {
	t.Helper()
	dir := t.TempDir()
	img := image{
		script:  filepath.Join(dir, "watcher-entrypoint.sh"),
		watcher: filepath.Join(dir, "watcher"),
	}
	content, err := os.ReadFile("watcher-entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(img.script, content, 0o755); err != nil {
		t.Fatal(err)
	}
	if withWatcher {
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(self, img.watcher); err != nil {
			t.Fatal(err)
		}
	}
	return img
}

// env returns the environment for a container whose config directory is
// dir. Empty means the operator has set nothing.
func env(dir string) []string {
	return append(os.Environ(), runMainEnv+"=1", envWatchDir+"="+dir, envDebounce+"=0")
}

// start returns a command that runs the start script with app as the
// command of the image.
func (img image) start(dir string, app ...string) *exec.Cmd {
	cmd := exec.Command(img.script, app...)
	cmd.Env = env(dir)
	return cmd
}

// healthcheck runs "watcher --healthcheck" and returns its exit code.
func (img image) healthcheck(t *testing.T, dir string) int {
	t.Helper()
	cmd := exec.Command(img.watcher, flagHealthcheck)
	cmd.Env = env(dir)
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.ExitCode()
	}
	t.Fatalf("healthcheck did not run: %v", err)
	return -1
}

// commandLine returns the command line of a process, with spaces between
// the words. It is empty when the process is gone or has ended.
func commandLine(pid int) string {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " "))
}

// childrenOf returns the PIDs of the processes whose parent is pid.
func childrenOf(t *testing.T, pid int) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	var children []int
	for _, e := range entries {
		child, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// The line is "pid (name) state ppid ...". The name can hold spaces
		// and brackets, so count the fields from the last ")".
		i := strings.LastIndex(string(stat), ") ")
		if i < 0 {
			continue
		}
		fields := strings.Fields(string(stat)[i+2:])
		if len(fields) > 1 && fields[1] == strconv.Itoa(pid) {
			children = append(children, child)
		}
	}
	return children
}

// waitForApp waits until the process has turned into the app.
func waitForApp(t *testing.T, pid int, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for commandLine(pid) != want {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d is %q, want the app %q", pid, commandLine(pid), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// watchProcessOf waits for the watch process next to the app with this PID,
// and returns its PID.
func (img image) watchProcessOf(t *testing.T, app int) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, child := range childrenOf(t, app) {
			if commandLine(child) == img.watcher {
				return child
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no watch process under pid %d", app)
	return 0
}

// stopAtEnd ends the app when the test is over.
func stopAtEnd(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
}

func TestEntrypointBecomesTheApp(t *testing.T) {
	cmd := newImage(t, true).start("", "sh", "-c", "echo $$; exit 7")
	out, err := cmd.Output()

	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("ended with %v, want the exit code 7 of the app", err)
	}
	// The app prints its own PID. It must be the PID we started: the script
	// did not start the app next to itself, it turned into the app.
	if got, want := strings.TrimSpace(string(out)), strconv.Itoa(cmd.ProcessState.Pid()); got != want {
		t.Fatalf("the app ran as pid %s, want %s", got, want)
	}
}

func TestEntrypointWithoutDirRunsTheAppAlone(t *testing.T) {
	cmd := newImage(t, true).start("", "sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, cmd)

	waitForApp(t, cmd.Process.Pid, "sleep 30")
	time.Sleep(200 * time.Millisecond)
	if children := childrenOf(t, cmd.Process.Pid); len(children) != 0 {
		t.Fatalf("the app has %d child processes, want none", len(children))
	}
}

func TestEntrypointStartsTheAppWhenTheWatcherCannotRun(t *testing.T) {
	// The image has the start script, but no watcher next to it.
	cmd := newImage(t, false).start(t.TempDir(), "sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, cmd)

	waitForApp(t, cmd.Process.Pid, "sleep 30")
}

func TestWatchProcessCanDieWithoutTheApp(t *testing.T) {
	img := newImage(t, true)
	dir := t.TempDir()
	cmd := img.start(dir, "sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, cmd)

	waitForApp(t, cmd.Process.Pid, "sleep 30")
	watchPid := img.watchProcessOf(t, cmd.Process.Pid)
	t.Cleanup(func() { _ = syscall.Kill(watchPid, syscall.SIGKILL) })
	if code := img.healthcheck(t, dir); code != 0 {
		t.Fatalf("healthcheck with a live watch process = %d, want 0", code)
	}

	// Kill the watch process. The healthcheck must notice, and the app must
	// not.
	if err := syscall.Kill(watchPid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for img.healthcheck(t, dir) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("healthcheck still succeeds after the watch process was killed")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := commandLine(cmd.Process.Pid); got != "sleep 30" {
		t.Fatalf("after the watch process died the app is %q, want it running", got)
	}
}

func TestHealthcheckWithoutDirSucceeds(t *testing.T) {
	if code := newImage(t, true).healthcheck(t, ""); code != 0 {
		t.Fatalf("healthcheck with no config directory = %d, want 0", code)
	}
}

func TestWatcherRefusesToStartAnApp(t *testing.T) {
	// This is how an image made for 0.1 calls the watcher.
	cmd := exec.Command(newImage(t, true).watcher, "--", "sleep", "30")
	cmd.Env = env("")
	out, err := cmd.CombinedOutput()

	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() == 0 {
		t.Fatalf("ended with %v, want a failure", err)
	}
	if !strings.Contains(string(out), "watcher-entrypoint.sh") {
		t.Fatalf("the message does not name the start script: %s", out)
	}
}

// This one waits for the real startup grace time, so it takes a few seconds.
func TestWatchProcessReloadsTheAppAndEndsWithIt(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the startup grace time")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, confName)
	if err := os.WriteFile(file, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The app says "reloaded" every time it gets SIGHUP.
	img := newImage(t, true)
	cmd := img.start(dir, "sh", "-c",
		`trap 'echo reloaded' HUP; echo ready; while :; do sleep 0.1; done`)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// The watch process writes its log to the same stderr as the app.
	logs, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	lines := make(chan string, 16)
	go func() {
		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	// The watch process has seen the config it starts from when it says
	// "watching". A change made before that would not count as a change.
	watching := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(logs)
		said := false
		for scanner.Scan() {
			if !said && strings.Contains(scanner.Text(), "watching ") {
				said = true
				close(watching)
			}
		}
	}()
	expect := func(want string, within time.Duration) {
		t.Helper()
		select {
		case got := <-lines:
			if got != want {
				t.Fatalf("the app said %q, want %q", got, want)
			}
		case <-time.After(within):
			t.Fatalf("the app did not say %q in %s", want, within)
		}
	}

	expect("ready", 3*time.Second)
	watchPid := img.watchProcessOf(t, cmd.Process.Pid)
	t.Cleanup(func() { _ = syscall.Kill(watchPid, syscall.SIGKILL) })
	select {
	case <-watching:
	case <-time.After(3 * time.Second):
		t.Fatal("the watch process did not say that it is watching")
	}

	if err := os.WriteFile(file, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	expect("reloaded", startupGrace+3*time.Second)

	// When the app ends, the watch process must not stay behind.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = cmd.Process.Wait()
	deadline := time.Now().Add(parentCheck + 3*time.Second)
	for commandLine(watchPid) != "" {
		if time.Now().After(deadline) {
			t.Fatalf("the watch process pid %d is still running after the app ended", watchPid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

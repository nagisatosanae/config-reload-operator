package main

import (
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// subscribeSigchld makes the test process a subreaper, so orphans come to it
// like they come to PID 1, and returns the SIGCHLD channel for reap. The
// channel is closed at the end of the test, which ends reap.
func subscribeSigchld(t *testing.T) chan os.Signal {
	t.Helper()
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Skipf("cannot become a subreaper: %v", err)
	}
	sigchld := make(chan os.Signal, 1)
	signal.Notify(sigchld, syscall.SIGCHLD)
	t.Cleanup(func() {
		signal.Stop(sigchld)
		close(sigchld)
	})
	return sigchld
}

func TestReapCollectsOrphansAndReportsTheApp(t *testing.T) {
	sigchld := subscribeSigchld(t)

	// The app leaves a child behind and ends with code 3. That child is now
	// an orphan, and it ends 0.2s later.
	app := exec.Command("sh", "-c", "sleep 0.2 & exit 3")
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan syscall.WaitStatus, 1)
	go reap(sigchld, app.Process.Pid, exited)

	select {
	case status := <-exited:
		if !status.Exited() || status.ExitStatus() != 3 {
			t.Fatalf("app status = %v, want exit code 3", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the end of the app was not reported")
	}

	// Give the orphan time to end and reap time to collect it. After that
	// there must be no child left at all. A zombie would still be returned.
	time.Sleep(500 * time.Millisecond)
	reapMu.Lock()
	defer reapMu.Unlock()
	pid, err := syscall.Wait4(-1, nil, syscall.WNOHANG, nil)
	if err != syscall.ECHILD {
		t.Fatalf("a child is still there: pid=%d err=%v", pid, err)
	}
}

func TestValidateKeepsItsExitCodeWhileReaping(t *testing.T) {
	sigchld := subscribeSigchld(t)
	app := exec.Command("sleep", "30")
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan syscall.WaitStatus, 1)
	go reap(sigchld, app.Process.Pid, exited)
	t.Cleanup(func() {
		_ = app.Process.Kill()
		<-exited
	})

	// reap and validate both wait for children. Without the lock, reap can
	// take the check command first, and validate then sees "no child
	// processes" instead of the real exit code. It takes a few hundred runs
	// to show up, so this runs many times.
	for i := range 1000 {
		err := validate([]string{"sh", "-c", "exit 7"}, time.Second)
		if err == nil || !strings.Contains(err.Error(), "exit status 7") {
			t.Fatalf("run %d: got %v, want exit status 7", i, err)
		}
	}
}

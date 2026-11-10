package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidateCommandFromEnv(t *testing.T) {
	t.Setenv(envValidate, "")
	if c, err := validateCommandFromEnv(); c != nil || err != nil {
		t.Fatalf("empty: got %v, %v", c, err)
	}

	t.Setenv(envValidate, `["nginx","-t"]`)
	c, err := validateCommandFromEnv()
	if err != nil || len(c) != 2 || c[0] != "nginx" || c[1] != "-t" {
		t.Fatalf("list: got %v, %v", c, err)
	}

	t.Setenv(envValidate, `["sh","-c","echo a b"]`)
	c, err = validateCommandFromEnv()
	if err != nil || len(c) != 3 || c[2] != "echo a b" {
		t.Fatalf("words with spaces: got %v, %v", c, err)
	}

	t.Setenv(envValidate, "nginx -t")
	if _, err := validateCommandFromEnv(); err == nil {
		t.Fatal("plain text must be an error, not a guess")
	}
}

func TestValidate(t *testing.T) {
	if err := validate(nil, time.Second); err != nil {
		t.Fatalf("no command: %v", err)
	}
	if err := validate([]string{"true"}, time.Second); err != nil {
		t.Fatalf("true: %v", err)
	}

	err := validate([]string{"sh", "-c", "echo bad line 4 >&2; exit 1"}, time.Second)
	if err == nil {
		t.Fatal("exit 1 must fail")
	}
	if !strings.Contains(err.Error(), "bad line 4") {
		t.Fatalf("the command output is missing from the error: %v", err)
	}

	if err := validate([]string{"no-such-command-here"}, time.Second); err == nil {
		t.Fatal("a missing command must fail")
	}

	start := time.Now()
	err = validate([]string{"sleep", "5"}, 200*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("timeout: got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("the timeout did not stop the command")
	}
}

// startWatchWithCheck is like startWatch but lets the test decide if the
// config is good.
func startWatchWithCheck(t *testing.T, dir string, good *atomic.Bool) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	check := func() error {
		if good.Load() {
			return nil
		}
		return errors.New("bad config")
	}
	go watch(dir, testQuiet, 0, check, func() { n.Add(1) })
	time.Sleep(50 * time.Millisecond)
	return &n
}

func TestWatchDoesNotReloadWhenCheckFails(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.conf")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("good v1")

	var good atomic.Bool
	n := startWatchWithCheck(t, dir, &good)

	// bad config: no reload
	good.Store(false)
	write("broken")
	time.Sleep(settle)
	if got := n.Load(); got != 0 {
		t.Fatalf("reloads after a bad config = %d, want 0", got)
	}

	// fixed with new content: reload once
	good.Store(true)
	write("good v2")
	time.Sleep(settle)
	if got := n.Load(); got != 1 {
		t.Fatalf("reloads after the fix = %d, want 1", got)
	}
}

func TestWatchGoingBackToRunningConfigDoesNothing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.conf")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("good v1")

	var good atomic.Bool
	n := startWatchWithCheck(t, dir, &good)

	good.Store(false)
	write("broken")
	time.Sleep(settle)

	// The user gives up and puts the old config back. The app never left
	// it, so there is nothing to reload.
	good.Store(true)
	write("good v1")
	time.Sleep(settle)
	if got := n.Load(); got != 0 {
		t.Fatalf("reloads after going back to the running config = %d, want 0", got)
	}
}

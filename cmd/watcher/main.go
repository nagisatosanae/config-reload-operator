package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"
)

// The controller sets these on the container. When WATCH_DIR is empty, the
// watcher only runs the app.
const (
	envWatchDir = "WATCH_DIR"
	envSignal   = "WATCH_SIGNAL"
	envDebounce = "WATCH_DEBOUNCE_SECONDS"
	envValidate = "WATCH_VALIDATE_COMMAND"
)

const (
	// defaultQuiet is how long the directory must stay quiet before we look
	// at it, when WATCH_DEBOUNCE_SECONDS does not say. One ConfigMap update
	// makes several events in a row, and this turns them into one check.
	defaultQuiet = 3 * time.Second

	// startupGrace is how long we wait after start before sending any
	// signal. Until the app is up, the child may still be a start script,
	// and a signal would kill it.
	startupGrace = 5 * time.Second

	// validateTimeout is how long the check command may run.
	validateTimeout = 10 * time.Second
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		log.Fatal("usage: watcher [--] COMMAND [ARGS...]")
	}

	// 1. Run the application as a child process, sharing stdin/stdout/stderr.
	// We ask for SIGCHLD before the start, so the end of a child is never
	// missed.
	sigchld := make(chan os.Signal, 1)
	signal.Notify(sigchld, syscall.SIGCHLD)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		log.Fatalf("start: %v", err)
	}
	log.Printf("watcher pid=%d started child pid=%d", os.Getpid(), cmd.Process.Pid)

	// 2. Collect every child that ends. This is the job of PID 1: without
	// it, an orphan that ends stays as a zombie. The end of the app itself
	// comes back on exited.
	exited := make(chan syscall.WaitStatus, 1)
	go reap(sigchld, cmd.Process.Pid, exited)

	// 3. Forward termination signals to the child.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGINT)
	go func() {
		for s := range sigs {
			log.Printf("got %v, forwarding to child", s)
			_ = cmd.Process.Signal(s)
		}
	}()

	// 4. Watch the config directory, if we were told which one.
	if dir := os.Getenv(envWatchDir); dir != "" {
		sig := signalFromEnv()
		quiet := quietFromEnv()
		command, commandErr := validateCommandFromEnv()
		log.Printf("watching %s (signal %s, quiet time %s)", dir, unix.SignalName(sig), quiet)
		switch {
		case commandErr != nil:
			log.Printf("config will not be reloaded until this is fixed: %v", commandErr)
		case len(command) == 0:
			log.Printf("%s is not set, so a new config is reloaded without a check", envValidate)
		default:
			log.Printf("a new config is checked with %q before the reload", command)
		}

		check := func() error {
			if commandErr != nil {
				return commandErr
			}
			return validate(command, validateTimeout)
		}
		reload := func() {
			log.Printf("sending %s", unix.SignalName(sig))
			if err := cmd.Process.Signal(sig); err != nil {
				log.Printf("could not send %s: %v", unix.SignalName(sig), err)
			}
		}
		go watch(dir, quiet, startupGrace, check, reload)
	} else {
		log.Printf("%s is not set, so config changes are not watched", envWatchDir)
	}

	// 5. Wait for the app to end, and exit with its code (128+N if it died
	// from a signal).
	status := <-exited
	code := status.ExitStatus()
	if status.Signaled() {
		code = 128 + int(status.Signal())
	}
	log.Printf("child exited with code %d", code)
	os.Exit(code)
}

// reapMu keeps reap away from a child that Go waits for itself, like the
// check command. Both of them wait, and only one can get the exit code.
var reapMu sync.Mutex

// reap collects every child that has ended. As PID 1 we are the parent of
// every orphan in the container, and an ended process that nobody collects
// stays as a zombie. When the app itself ends, its status is sent on exited.
func reap(sigchld <-chan os.Signal, app int, exited chan<- syscall.WaitStatus) {
	for range sigchld {
		reapMu.Lock()
		for {
			var status syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
			if err == syscall.EINTR {
				continue
			}
			if pid <= 0 || err != nil {
				break // 0: nothing has ended yet. ECHILD: there is no child at all.
			}
			if pid == app {
				exited <- status
				continue
			}
			log.Printf("collected orphan pid=%d", pid)
		}
		reapMu.Unlock()
	}
}

// signalFromEnv returns the signal named in WATCH_SIGNAL. It returns SIGHUP
// when the name is missing or not a real signal.
func signalFromEnv() syscall.Signal {
	name := os.Getenv(envSignal)
	if name == "" {
		return syscall.SIGHUP
	}
	sig := unix.SignalNum(name)
	if sig == 0 {
		log.Printf("unknown signal %q in %s, using SIGHUP", name, envSignal)
		return syscall.SIGHUP
	}
	return sig
}

// quietFromEnv returns the quiet time set in WATCH_DEBOUNCE_SECONDS. It
// returns the default when the value is missing or not a number 0 or above.
func quietFromEnv() time.Duration {
	value := os.Getenv(envDebounce)
	if value == "" {
		return defaultQuiet
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 0 {
		log.Printf("bad value %q in %s, using %s", value, envDebounce, defaultQuiet)
		return defaultQuiet
	}
	return time.Duration(seconds) * time.Second
}

// validateCommandFromEnv returns the check command from
// WATCH_VALIDATE_COMMAND. The value is a JSON list of words, for example
// ["nginx","-t"]. An empty value means there is no check.
func validateCommandFromEnv() ([]string, error) {
	value := os.Getenv(envValidate)
	if value == "" {
		return nil, nil
	}
	var command []string
	if err := json.Unmarshal([]byte(value), &command); err != nil {
		return nil, fmt.Errorf("cannot read %s %q: %w", envValidate, value, err)
	}
	return command, nil
}

// validate runs the check command and returns nil when the new config may
// be used. With no command there is nothing to check.
func validate(command []string, timeout time.Duration) error {
	if len(command) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	// If the command leaves a process behind that keeps our output pipe
	// open, stop waiting for the output after a moment.
	cmd.WaitDelay = time.Second

	// Go waits for this child itself, so reap must stay away from it.
	reapMu.Lock()
	defer reapMu.Unlock()

	out, err := cmd.CombinedOutput()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%s did not finish in %s", command[0], timeout)
	}
	if err != nil {
		return fmt.Errorf("%s: %w: %s", command[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

// watch calls reload when the content of the files in dir has changed and
// check says the new content is good.
//
// The kernel tells us when something happens in dir, so there is no polling.
// Events only start a timer. When dir has been quiet for the quiet time, we
// compare the content with what the app is running, and go on only if it is
// different. Nothing is reloaded before the grace time has passed; a change
// seen earlier is kept and handled right after it.
func watch(dir string, quiet, grace time.Duration, check func() error, reload func()) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("cannot start watching: %v", err)
		return
	}
	defer func() { _ = w.Close() }()

	if err := w.Add(dir); err != nil {
		log.Printf("cannot watch %s: %v", dir, err)
		return
	}

	started := time.Now()
	// running is the hash of the config the app is running. It only moves
	// forward when a reload was really sent.
	running := hashDir(dir)
	events := 0

	timer := time.NewTimer(time.Hour)
	timer.Stop()

	for {
		select {
		case _, ok := <-w.Events:
			if !ok {
				return
			}
			events++
			timer.Reset(quiet)

		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			log.Printf("watch error: %v", err)

		case <-timer.C:
			// Too early to signal. Come back when the grace time is over.
			if wait := grace - time.Since(started); wait > 0 {
				timer.Reset(wait)
				continue
			}

			h := hashDir(dir)
			if h == running {
				log.Printf("%d events, content is the same", events)
				events = 0
				continue
			}
			log.Printf("%d events, content changed", events)
			events = 0

			if err := check(); err != nil {
				log.Printf("check failed, not reloading: %v", err)
				continue
			}
			running = h
			reload()
		}
	}
}

// hashDir returns one hash for the names and content of all files in dir.
//
// It reads files the way the app does: it follows symlinks, because a
// ConfigMap volume is made of them. It skips names that start with "..",
// which the kubelet uses for its own bookkeeping.
func hashDir(dir string) string {
	h := sha256.New()
	hashInto(h, dir, "", 0)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// maxDepth stops hashInto if symlinks ever form a loop.
const maxDepth = 16

// hashInto adds every file under root/rel to h.
func hashInto(h hash.Hash, root, rel string, depth int) {
	if depth > maxDepth {
		return
	}
	entries, err := os.ReadDir(filepath.Join(root, rel))
	if err != nil {
		return
	}
	// ReadDir returns the entries sorted by name, so the hash is stable.
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "..") {
			continue
		}
		name := filepath.Join(rel, e.Name())
		path := filepath.Join(root, name)

		info, err := os.Stat(path) // Stat follows symlinks.
		if err != nil {
			continue
		}
		if info.IsDir() {
			hashInto(h, root, name, depth+1)
			continue
		}

		f, err := os.Open(path)
		if err != nil {
			continue
		}
		// The zero bytes keep "name + content" from running together.
		_, _ = io.WriteString(h, name+"\x00")
		_, _ = io.Copy(h, f)
		_, _ = h.Write([]byte{0})
		_ = f.Close()
	}
}

package main

import (
	"crypto/sha256"
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
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"
)

// container. When it is empty, the watcher only runs the app.
// The controller sets these on the container. When WATCH_DIR is empty, the
// watcher only runs the app.
const (
	envWatchDir = "WATCH_DIR"
	envSignal   = "WATCH_SIGNAL"
	envDebounce = "WATCH_DEBOUNCE_SECONDS"
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
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		log.Fatalf("start: %v", err)
	}
	log.Printf("watcher pid=%d started child pid=%d", os.Getpid(), cmd.Process.Pid)

	// 2. Forward termination signals to the child.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGINT)
	go func() {
		for s := range sigs {
			log.Printf("got %v, forwarding to child", s)
			_ = cmd.Process.Signal(s)
		}
	}()

	// 3. Watch the config directory, if we were told which one.
	// 3. Watch the config directory, if we were told which one.
	if dir := os.Getenv(envWatchDir); dir != "" {
		sig := signalFromEnv()
		quiet := quietFromEnv()
		log.Printf("watching %s (signal %s, quiet time %s)", dir, unix.SignalName(sig), quiet)
		go watch(dir, quiet, startupGrace, func() {
			log.Printf("sending %s", unix.SignalName(sig))
			if err := cmd.Process.Signal(sig); err != nil {
				log.Printf("could not send %s: %v", unix.SignalName(sig), err)
			}
		})
	} else {
		log.Printf("%s is not set, so config changes are not watched", envWatchDir)
	}

	// 4. Exit with the child's exit code (128+N if it died from a signal).
	err := cmd.Wait()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = 128 + int(ws.Signal())
		}
	} else if err != nil {
		code = 1
	}
	log.Printf("child exited with code %d", code)
	os.Exit(code)
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

// watch calls reload when the content of the files in dir has changed.
//
// The kernel tells us when something happens in dir, so there is no polling.
// Events only start a timer. When dir has been quiet for the quiet time, we
// compare the content with what we saw last, and call reload only if it is
// different. Nothing is reloaded before the grace time has passed; a change
// seen earlier is kept and handled right after it.
func watch(dir string, quiet, grace time.Duration, reload func()) {
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
	last := hashDir(dir)
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
			if h == last {
				log.Printf("%d events, content is the same", events)
				events = 0
				continue
			}
			log.Printf("%d events, content changed", events)
			last = h
			events = 0
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

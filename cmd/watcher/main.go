package main

import (
	"bytes"
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
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"
)

// This binary runs in two ways:
//
//	watcher                the watch process
//	watcher --healthcheck  for an exec probe: is the watch process alive?
//
// It does not start the app. The start script watcher-entrypoint.sh does
// that: it starts the watch process and then turns into the app. So the app
// is PID 1 in the container, and the watch process is its child. The app
// gets its own signals and ends with its own exit code, the same as in an
// image without the watcher, and a watch process that fails can never take
// the app down with it.
const flagHealthcheck = "--healthcheck"

// The operator sets these on the container. When CONFIG_RELOAD_DIR is empty,
// the start script starts no watch process and the container runs the app
// alone.
const (
	envWatchDir = "CONFIG_RELOAD_DIR"
	envSignal   = "CONFIG_RELOAD_SIGNAL"
	envDebounce = "CONFIG_RELOAD_DEBOUNCE_SECONDS"
	envValidate = "CONFIG_RELOAD_VALIDATE_COMMAND"
)

const (
	// defaultQuiet is how long the directory must stay quiet before we look
	// at it, when CONFIG_RELOAD_DEBOUNCE_SECONDS does not say. One ConfigMap
	// update makes several events in a row, and this turns them into one
	// check.
	defaultQuiet = 3 * time.Second

	// startupGrace is how long we wait after start before sending any
	// signal. Until the app is up, the process we signal may still be a
	// start script, and a signal could end it.
	startupGrace = 5 * time.Second

	// validateTimeout is how long the check command may run.
	validateTimeout = 10 * time.Second

	// parentCheck is how often the watch process looks if the app is still
	// its parent.
	parentCheck = 2 * time.Second

	// When watching stops, we try again after retryMin, and wait twice as
	// long after every failure, up to retryMax.
	retryMin = time.Second
	retryMax = 5 * time.Minute
)

func main() {
	args := os.Args[1:]
	switch {
	case len(args) == 0:
		runWatch()
	case len(args) == 1 && args[0] == flagHealthcheck:
		os.Exit(healthcheck())
	default:
		// Up to 0.1 the watcher started the app itself: "watcher -- COMMAND".
		// Say so clearly, or an old image would run with no app in it.
		log.Fatalf("unknown arguments %q: the watcher no longer starts the app, "+
			"make watcher-entrypoint.sh the start command of the image", args)
	}
}

// runWatch is the watch process. Its parent is the app. It watches the
// config directory for as long as the app lives and sends the app a signal
// when the content has changed.
func runWatch() {
	// The signal goes to our parent. The first process of a container has
	// none, so being that process is a mistake in the image.
	if os.Getpid() == 1 {
		log.Fatal("the watcher must not be the start command of the image, " +
			"use watcher-entrypoint.sh: it starts the watcher next to the app")
	}
	app := os.Getppid()
	dir := os.Getenv(envWatchDir)
	if dir == "" {
		log.Fatalf("%s is not set, there is nothing to watch", envWatchDir)
	}

	// Take the hash first: the app is starting right now and reads the same
	// files. A change made after this point is a change for the app too.
	keep := &memory{started: time.Now(), running: hashDir(dir)}

	// Leave the process group of the app, so a signal the app sends to its
	// whole group does not reach us. It fails when we already lead a group,
	// and then there is nothing to do.
	_ = syscall.Setpgid(0, 0)

	sig := signalFromEnv()
	quiet := quietFromEnv()
	command, commandErr := validateCommandFromEnv()
	log.Printf("watching %s for pid %d (signal %s, quiet time %s)", dir, app, unix.SignalName(sig), quiet)
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
		// If the app is gone, its PID may belong to someone else by now.
		if os.Getppid() != app {
			return
		}
		log.Printf("sending %s", unix.SignalName(sig))
		if err := syscall.Kill(app, sig); err != nil {
			log.Printf("could not send %s: %v", unix.SignalName(sig), err)
		}
	}

	// In a container the kernel ends every process when PID 1 ends. This is
	// for all other cases: when the app is no longer our parent, we are done.
	go func() {
		for range time.Tick(parentCheck) {
			if os.Getppid() != app {
				log.Printf("the app is gone, the watch process stops")
				os.Exit(0)
			}
		}
	}()

	keepWatching(keep, dir, quiet, startupGrace, check, reload, retryMin, retryMax)
}

// healthcheck is for an exec probe. It tells if the watch process of this
// container is alive: exit code 0 for yes, 1 for no. With no config
// directory set, no watch process is expected, and that is fine too.
//
// It only sees a watch process that is gone. It cannot see one that hangs.
func healthcheck() int {
	if os.Getenv(envWatchDir) == "" {
		return 0
	}
	if watchProcessAlive() {
		return 0
	}
	fmt.Fprintln(os.Stderr, "the watch process is not running")
	return 1
}

// watchProcessAlive looks through /proc for a process that runs this same
// binary with no arguments. That is how the watch process is started.
func watchProcessAlive() bool {
	self, err := os.Readlink("/proc/self/exe")
	if err != nil {
		return false
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue // not a process
		}
		// A process that has ended but was not collected has no exe link.
		exe, err := os.Readlink(filepath.Join("/proc", e.Name(), "exe"))
		if err != nil || exe != self {
			continue
		}
		// The words of the command line each end with a zero byte. One word
		// means no arguments.
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err == nil && bytes.Count(cmdline, []byte{0}) == 1 {
			return true
		}
	}
	return false
}

// signalFromEnv returns the signal named in CONFIG_RELOAD_SIGNAL. It returns
// SIGHUP when the name is missing or not a real signal.
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

// quietFromEnv returns the quiet time set in CONFIG_RELOAD_DEBOUNCE_SECONDS.
// It returns the default when the value is missing or not a number 0 or
// above.
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
// CONFIG_RELOAD_VALIDATE_COMMAND. The value is a JSON list of words, for
// example ["nginx","-t"]. An empty value means there is no check.
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

	out, err := cmd.CombinedOutput()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%s did not finish in %s", command[0], timeout)
	}
	if err != nil {
		return fmt.Errorf("%s: %w: %s", command[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

// memory is what must not be lost when watching stops and starts again.
type memory struct {
	// started is when we began. Nothing is reloaded before the grace time
	// after it has passed.
	started time.Time
	// running is the hash of the config the app was last told to load. It
	// moves forward when a new config has passed the check, right before
	// the signal is sent.
	running string
}

// keepWatching watches dir for as long as this process lives.
//
// Watching can stop: the directory is not there, the kernel has no more
// watches to give, or there is a bug in our own code. The app must not
// notice any of that. So we write a line to the log, wait and try again.
// keep is carried over, so a change that was made in between is still seen.
func keepWatching(
	keep *memory, dir string, quiet, grace time.Duration,
	check func() error, reload func(), minWait, maxWait time.Duration,
) {
	wait := minWait
	for {
		began := time.Now()
		watchSafely(keep, dir, quiet, grace, check, reload)
		// A long run means the earlier trouble was over. Start small again.
		if time.Since(began) > maxWait {
			wait = minWait
		}
		log.Printf("watching stopped, trying again in %s", wait)
		time.Sleep(wait)
		wait = min(wait*2, maxWait)
	}
}

// watchSafely runs watchOnce and turns a panic into a log line.
func watchSafely(keep *memory, dir string, quiet, grace time.Duration, check func() error, reload func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("watching stopped by a bug: %v", r)
		}
	}()
	watchOnce(keep, dir, quiet, grace, check, reload)
}

// watchOnce is the watch loop.
//
// The kernel tells us when something happens in dir, so there is no polling.
// Events only start a timer. When dir has been quiet for the quiet time, we
// compare the content with what the app is running, and go on only if it is
// different. Nothing is reloaded before the grace time has passed; a change
// seen earlier is kept and handled right after it.
func watchOnce(keep *memory, dir string, quiet, grace time.Duration, check func() error, reload func()) {
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

	events := 0

	// The first look comes without an event. It finds a change that was made
	// while nobody was watching.
	timer := time.NewTimer(quiet)

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
			if wait := grace - time.Since(keep.started); wait > 0 {
				timer.Reset(wait)
				continue
			}

			h := hashDir(dir)
			if h == keep.running {
				if events > 0 {
					log.Printf("%d events, content is the same", events)
				}
				events = 0
				continue
			}
			log.Printf("%d events, content changed", events)
			events = 0

			if err := check(); err != nil {
				log.Printf("check failed, not reloading: %v", err)
				continue
			}
			keep.running = h
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

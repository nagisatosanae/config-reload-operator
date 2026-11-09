package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// envWatchDir names the directory to watch. The controller sets it on the
// container. When it is empty, the watcher only runs the app.
const envWatchDir = "WATCH_DIR"

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

	go func() {
		// 3. Watch the config directory, if we were told which one.
		if dir := os.Getenv(envWatchDir); dir != "" {
			if _, err := os.Stat(dir); err != nil {
				log.Printf("cannot read %s: %v", dir, err)
			}
			log.Printf("watching %s", dir)
			go watch(dir, cmd.Process)
		} else {
			log.Printf("%s is not set, so config changes are not watched", envWatchDir)
		}
	}()

	// 3. Exit with the child's exit code (128+N if it died from a signal).
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

// watch looks at dir once a second and sends SIGHUP to the app when the
// content of its files changes.
func watch(dir string, app *os.Process) {
	last := hashDir(dir)
	for range time.Tick(time.Second) {
		h := hashDir(dir)
		if h == last {
			continue
		}
		last = h
		log.Println("change detected, sending SIGHUP")
		if err := app.Signal(syscall.SIGHUP); err != nil {
			log.Printf("could not send SIGHUP: %v", err)
		}
	}
}

func hashDir(dir string) string {
	h := sha256.New()
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer func() { _ = f.Close() }()
		_, _ = io.WriteString(h, p)
		_, _ = io.Copy(h, f)
		return nil
	})
	return fmt.Sprintf("%x", h.Sum(nil))
}

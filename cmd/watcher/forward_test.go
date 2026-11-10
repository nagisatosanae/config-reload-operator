package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// signalChildEnv turns the test binary into a stand-in for the app. See
// TestMain.
const signalChildEnv = "WATCHER_TEST_SIGNAL_CHILD"

// TestMain lets the test binary play the app. When signalChildEnv is set, it
// runs no tests: it waits for one signal and exits with 100 plus the number
// of that signal. The forward test starts it that way, so the test needs no
// shell and leaves no other process behind.
func TestMain(m *testing.M) {
	if os.Getenv(signalChildEnv) == "1" {
		got := make(chan os.Signal, 1)
		signal.Notify(got, forwarded...)
		fmt.Println("ready")
		os.Exit(100 + int((<-got).(syscall.Signal)))
	}
	os.Exit(m.Run())
}

func TestForwardPassesEverySignalToTheApp(t *testing.T) {
	for _, s := range forwarded {
		sig := s.(syscall.Signal)
		t.Run(unix.SignalName(sig), func(t *testing.T) {
			app := exec.Command(os.Args[0])
			app.Env = append(os.Environ(), signalChildEnv+"=1")
			out, err := app.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := app.Start(); err != nil {
				t.Fatal(err)
			}
			// The child prints one line when it is ready to take signals.
			if _, err := bufio.NewReader(out).ReadString('\n'); err != nil {
				t.Fatalf("the child did not get ready: %v", err)
			}

			// One signal in, then the channel is closed so forward returns.
			sigs := make(chan os.Signal, 1)
			sigs <- sig
			close(sigs)
			forward(sigs, app.Process)

			var exit *exec.ExitError
			if err := app.Wait(); !errors.As(err, &exit) {
				t.Fatalf("the child ended with %v, want an exit code", err)
			}
			if want := 100 + int(sig); exit.ExitCode() != want {
				t.Fatalf("exit code = %d, want %d: the signal did not arrive", exit.ExitCode(), want)
			}
		})
	}
}

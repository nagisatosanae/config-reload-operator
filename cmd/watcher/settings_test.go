package main

import (
	"syscall"
	"testing"
	"time"
)

func TestSignalFromEnv(t *testing.T) {
	cases := []struct {
		value string
		want  syscall.Signal
	}{
		{"", syscall.SIGHUP},
		{"SIGHUP", syscall.SIGHUP},
		{"SIGUSR1", syscall.SIGUSR1},
		{"SIGUSR2", syscall.SIGUSR2},
		{"sighup", syscall.SIGHUP},  // not a real name, falls back
		{"NOPE", syscall.SIGHUP},    // unknown, falls back
		{"SIGUSR9", syscall.SIGHUP}, // unknown, falls back
	}
	for _, c := range cases {
		t.Setenv(envSignal, c.value)
		if got := signalFromEnv(); got != c.want {
			t.Errorf("%s=%q: got %v, want %v", envSignal, c.value, got, c.want)
		}
	}
}

func TestQuietFromEnv(t *testing.T) {
	cases := []struct {
		value string
		want  time.Duration
	}{
		{"", defaultQuiet},
		{"0", 0},
		{"1", time.Second},
		{"10", 10 * time.Second},
		{"-1", defaultQuiet},
		{"abc", defaultQuiet},
		{"1.5", defaultQuiet},
	}
	for _, c := range cases {
		t.Setenv(envDebounce, c.value)
		if got := quietFromEnv(); got != c.want {
			t.Errorf("%s=%q: got %v, want %v", envDebounce, c.value, got, c.want)
		}
	}
}

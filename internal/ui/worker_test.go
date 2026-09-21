package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestStartDisabledIsNoop(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	w := newUIWorker(NewDefaultConfig(), "", zap.New(core))
	if err := w.start(nil); err != nil {
		t.Fatalf("disabled ui should not error on start: %v", err)
	}
	if countLogs(logs, "ui started") != 0 {
		t.Fatal("disabled ui must not start the supervisor")
	}
}

// Stop before start (SIGTERM during an earlier worker's OnStart) must leave
// nothing behind: the child would otherwise outlive the core.
func TestStopBeforeStartSpawnsNothing(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	w := newUIWorker(fakeChild(t, "echo spawned"), "", zap.New(core))
	if err := w.stop(nil); err != nil {
		t.Fatal(err)
	}
	if err := w.start(nil); err != nil {
		t.Fatal(err)
	}
	w.wg.Wait()
	if countLogs(logs, "spawned") != 0 {
		t.Fatal("child was spawned after stop")
	}
}

func TestCoreURL(t *testing.T) {
	for in, want := range map[string]string{":3333": "http://127.0.0.1:3333", "0.0.0.0:3333": "http://127.0.0.1:3333", "10.0.0.1:4444": "http://10.0.0.1:4444"} {
		if got := coreURL(in); got != want {
			t.Errorf("coreURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeChild stands in for python: it ignores the uvicorn argv and runs body.
func fakeChild(t *testing.T, body string) Config {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "python")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := NewDefaultConfig()
	cfg.Enabled, cfg.Dir, cfg.Python, cfg.RestartBackoff = true, dir, script, 10*time.Millisecond
	return cfg
}

func countLogs(logs *observer.ObservedLogs, msg string) int {
	n := 0
	for _, e := range logs.All() {
		if strings.Contains(e.Message, msg) {
			n++
		}
	}
	return n
}

func TestSuperviseLogsOutputAndRestarts(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	w := newUIWorker(fakeChild(t, "echo out-line; echo err-line >&2; exit 1"), "", zap.New(core))
	if err := w.start(nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for countLogs(logs, "out-line") < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := w.stop(nil); err != nil {
		t.Fatal(err)
	}
	if countLogs(logs, "out-line") < 2 || countLogs(logs, "err-line") < 1 {
		t.Fatalf("expected restarts with both streams logged, got %d stdout / %d stderr lines", countLogs(logs, "out-line"), countLogs(logs, "err-line"))
	}
	if countLogs(logs, "ui exited; restarting") < 1 {
		t.Fatal("expected a restart log line")
	}
}

func TestStopTerminatesChild(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	// Child ignores nothing: it exits on TERM, proving the signal path, not WaitDelay.
	w := newUIWorker(fakeChild(t, "trap 'echo got-term; exit 0' TERM; echo up; while :; do sleep 0.05; done"), "", zap.New(core))
	if err := w.start(nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for countLogs(logs, "up") < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	started := time.Now()
	if err := w.stop(nil); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(started); d > 5*time.Second {
		t.Fatalf("stop took %v; child was not terminated by SIGTERM", d)
	}
	if countLogs(logs, "got-term") != 1 {
		t.Fatalf("expected exactly one got-term line, got %d", countLogs(logs, "got-term"))
	}
	if countLogs(logs, "ui exited; restarting") != 0 {
		t.Fatal("child must not be restarted during stop")
	}
}

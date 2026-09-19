package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLiveShellProtocol(t *testing.T) {
	live := startTestLiveShell(t, t.TempDir())

	got, err := runLiveJob(context.Background(), live, "printf one")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got.Stdout) != "one" || got.Exit != 0 {
		t.Fatalf("first job: %+v", got)
	}
	got, err = runLiveJob(context.Background(), live, "printf two; printf err >&2; exit 7")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got.Stdout) != "twoerr" || got.Exit != 7 {
		t.Fatalf("second job: %+v", got)
	}
}

func TestLiveShellDoesNotPersistCommandEnvironment(t *testing.T) {
	root := t.TempDir()
	live := startTestLiveShell(t, root)
	if _, err := runLiveJob(context.Background(), live, "cd /; export PIGO_TEST_VALUE=changed"); err != nil {
		t.Fatal(err)
	}
	got, err := runLiveJob(context.Background(), live, `printf '%s|%s' "$PWD" "${PIGO_TEST_VALUE-unset}"`)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stdout != root+"|unset" {
		t.Fatalf("command state leaked into next run: %q", got.Stdout)
	}
}

func TestLiveShellCancellationStopsSandbox(t *testing.T) {
	live := startTestLiveShell(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := runLiveJob(ctx, live, "sleep 30"); err != context.DeadlineExceeded {
		t.Fatalf("run error = %v, want deadline exceeded", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for live.alive() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if live.alive() {
		t.Fatal("sandbox still alive after command cancellation")
	}
}

func TestLiveShellHandlesLargeOutputAndMarkerLikeText(t *testing.T) {
	live := startTestLiveShell(t, t.TempDir())
	got, err := runLiveJob(context.Background(), live, `printf '\036PIGO_DONE_not-our-token:9\037'; head -c 40000 /dev/zero | tr '\0' x`)
	if err != nil {
		t.Fatal(err)
	}
	if got.Exit != 0 {
		t.Fatalf("exit = %d", got.Exit)
	}
	if !strings.Contains(got.Stdout, "PIGO_DONE_not-our-token") || !strings.Contains(got.Stdout, "[truncated]") {
		t.Fatalf("unexpected bounded output: len=%d prefix=%q", len(got.Stdout), got.Stdout[:min(len(got.Stdout), 80)])
	}
	if len(got.Stdout) > outputHeadBytes+outputTailBytes+32 {
		t.Fatalf("bounded output is too large: %d", len(got.Stdout))
	}
}

func TestClearLegacyIPC(t *testing.T) {
	runDir := t.TempDir()
	for _, name := range []string{"supervisor.sh", "job.sh", "start", "done", "exit", "cancel", "pid", "ready", "stdout", "stderr", "supervisor.log"} {
		if err := os.WriteFile(filepath.Join(runDir, name), []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(runDir, "sandbox.log"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	clearLegacyIPC(runDir)
	entries, err := os.ReadDir(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "sandbox.log" {
		t.Fatalf("legacy IPC cleanup left %v", entries)
	}
}

func startTestLiveShell(t *testing.T, dir string) *liveSandbox {
	t.Helper()
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh")
	cmd.Dir = dir
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = stdoutW
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = stdinR.Close()
	_ = stdoutW.Close()
	live := &liveSandbox{cmd: cmd, stdin: stdinW, stdout: stdoutR, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(live.done)
	}()
	t.Cleanup(func() { live.stop() })
	return live
}

func TestReapIdleStopsProcessKeepsDisk(t *testing.T) {
	server := newTestServer(t)
	server.config.idleTimeout = time.Second
	id := createSession(t, server)
	managed, _ := server.getSession(id)
	note := filepath.Join(managed.paths.Workspace, "keep.txt")
	if err := os.WriteFile(note, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	live := &liveSandbox{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(live.done)
	}()
	managed.mu.Lock()
	managed.live = live
	managed.meta.LastUsed = time.Now().Add(-time.Hour)
	managed.mu.Unlock()

	server.reapIdle()

	managed.mu.Lock()
	alive := managed.liveAlive()
	managed.mu.Unlock()
	if alive {
		t.Fatal("expected idle sandbox to stop")
	}
	data, err := os.ReadFile(note)
	if err != nil || string(data) != "ok" {
		t.Fatalf("workspace should survive idle expiry: %v %q", err, data)
	}
}

func TestCloseKeepsSessionDir(t *testing.T) {
	server := newTestServer(t)
	id := createSession(t, server)
	managed, _ := server.getSession(id)
	note := filepath.Join(managed.paths.Workspace, "keep.txt")
	if err := os.WriteFile(note, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	server.close()
	if _, err := os.ReadFile(note); err != nil {
		t.Fatalf("close must keep disk state: %v", err)
	}
}

func TestLoadSessionsFromDisk(t *testing.T) {
	data := t.TempDir()
	id := "abc123"
	paths := newSessionPaths(data, id)
	if err := paths.create(); err != nil {
		t.Fatal(err)
	}
	meta := sessionMeta{ID: id, Model: "m", Provider: "p", Thinking: "low", LastUsed: time.Now().UTC()}
	if err := paths.saveMeta(meta); err != nil {
		t.Fatal(err)
	}
	got := loadSessions(data)
	if got[id] == nil || got[id].meta.Model != "m" {
		t.Fatalf("loadSessions = %+v", got)
	}
}

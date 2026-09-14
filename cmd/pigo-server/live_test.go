package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestIPCJobProtocol(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "workspace")
	home := filepath.Join(root, "home")
	runDir := filepath.Join(root, "run")
	for _, dir := range []string{ws, filepath.Join(home, ".pigo"), runDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeSupervisor(runDir); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", filepath.Join(runDir, "supervisor.sh"))
	cmd.Dir = ws
	cmd.Env = []string{
		"IPC=" + runDir,
		"HOME=" + home,
		"PIGO_HOME=" + filepath.Join(home, ".pigo"),
		"WORKSPACE=" + ws,
		"PATH=" + os.Getenv("PATH"),
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer killProcessGroup(cmd)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(runDir, "ready")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	got, err := runIPCJob(context.Background(), runDir, "#!/bin/sh\nprintf one\n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got.Stdout) != "one" || got.Exit != 0 {
		t.Fatalf("first job: %+v", got)
	}
	got, err = runIPCJob(context.Background(), runDir, "#!/bin/sh\nprintf two\n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got.Stdout) != "two" {
		t.Fatalf("second job: %+v", got)
	}
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	managed.mu.Lock()
	managed.live = cmd
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

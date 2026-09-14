package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxArgvIsolatesHomeAndClearsEnv(t *testing.T) {
	s := Sandbox{Bwrap: "/usr/bin/bwrap", Pigo: "/usr/local/bin/pigo"}
	argv, err := s.argv(RunSpec{
		Workspace: "/tmp/ws",
		Home:      "/tmp/home",
		Prompt:    "hi",
		Model:     "openrouter/free",
		Thinking:  "low",
		NoTools:   true,
		NoSkills:  true,
		Env:       []string{"OPENROUTER_API_KEY=secret", "HOME=/should-not-win"},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, "\n")
	if !containsArg(argv, "--clearenv") {
		t.Fatal("expected --clearenv")
	}
	if !containsTriple(argv, "--bind", "/tmp/ws", "/workspace") {
		t.Fatal("expected workspace bind")
	}
	if !containsTriple(argv, "--bind", "/tmp/home", "/home/pigo") {
		t.Fatal("expected home bind")
	}
	if strings.Contains(joined, os.Getenv("HOME")) && os.Getenv("HOME") != "" && os.Getenv("HOME") != "/tmp/home" {
		t.Fatalf("host HOME leaked into argv:\n%s", joined)
	}
	if !containsTriple(argv, "--setenv", "OPENROUTER_API_KEY", "secret") {
		t.Fatal("expected provider key")
	}
	if containsTriple(argv, "--setenv", "HOME", "/should-not-win") {
		t.Fatal("spec Env must not override sandbox HOME")
	}
	if !containsArg(argv, "--no-tools") || !containsArg(argv, "--approve") {
		t.Fatal("expected --no-tools and --approve")
	}
	if !containsPair(argv, "--thinking-level", "low") {
		t.Fatal("expected thinking level")
	}
}

func TestSandboxArgvResume(t *testing.T) {
	s := Sandbox{Bwrap: "/bin/bwrap", Pigo: "/bin/pigo"}
	argv, err := s.argv(RunSpec{
		Workspace:    "/ws",
		Home:         "/home",
		Prompt:       "next",
		ResumeID:     "abc",
		AllowedTools: []string{"read", "grep"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !containsPair(argv, "--resume", "abc") {
		t.Fatalf("missing resume: %v", argv)
	}
	if !containsPair(argv, "--allowed-tools", "read,grep") {
		t.Fatal("missing allowlist")
	}
}

func TestSandboxReadyFailClosed(t *testing.T) {
	s := Sandbox{Bwrap: filepath.Join(t.TempDir(), "missing")}
	if err := s.Ready(); err == nil {
		t.Fatal("expected ready error")
	}
}

func containsArg(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

func containsPair(argv []string, a, b string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == a && argv[i+1] == b {
			return true
		}
	}
	return false
}

func containsTriple(argv []string, a, b, c string) bool {
	for i := 0; i+2 < len(argv); i++ {
		if argv[i] == a && argv[i+1] == b && argv[i+2] == c {
			return true
		}
	}
	return false
}

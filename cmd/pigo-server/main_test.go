package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveWebInputAction(t *testing.T) {
	meta := &sessionMeta{Model: "openrouter/free", Provider: "openrouter", Thinking: "medium"}
	prompt, message, complete, err := resolveWebInput(meta, "/think low")
	if err != nil {
		t.Fatalf("resolveWebInput: %v", err)
	}
	if prompt != "" || !complete || !strings.Contains(message, "low") {
		t.Fatalf("got prompt=%q message=%q complete=%v", prompt, message, complete)
	}
	if meta.Thinking != "low" {
		t.Fatalf("thinking = %q", meta.Thinking)
	}
}

func TestResolveWebInputDoesNotSendUnsupportedCommandToModel(t *testing.T) {
	meta := &sessionMeta{Model: "m", Provider: "p", Thinking: "medium"}
	prompt, message, complete, err := resolveWebInput(meta, "/compact")
	if err != nil {
		t.Fatalf("resolveWebInput: %v", err)
	}
	if prompt != "" || !complete || !strings.Contains(message, "Web") {
		t.Fatalf("got prompt=%q message=%q complete=%v", prompt, message, complete)
	}
}

func TestResolveWebInputUnknownGoesToPrompt(t *testing.T) {
	meta := &sessionMeta{}
	prompt, _, complete, err := resolveWebInput(meta, "/not-a-command")
	if err != nil {
		t.Fatal(err)
	}
	if complete || prompt != "/not-a-command" {
		t.Fatalf("got prompt=%q complete=%v", prompt, complete)
	}
}

func TestHandleMessageStreamsHostLoopStub(t *testing.T) {
	server := newTestServer(t)
	id := createSession(t, server)

	body, _ := json.Marshal(messageRequest{Prompt: "hello"})
	request := httptest.NewRequest(http.MethodPost, "/api/sessions/"+id+"/messages?stream=true", bytes.NewReader(body))
	request.SetPathValue("id", id)
	response := httptest.NewRecorder()
	server.handleMessage(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
	out := response.Body.String()
	if !strings.Contains(out, `"type":"delta"`) || !strings.Contains(out, "hello:hello") {
		t.Fatalf("stream = %s", out)
	}

	body, _ = json.Marshal(messageRequest{Prompt: "again"})
	request = httptest.NewRequest(http.MethodPost, "/api/sessions/"+id+"/messages", bytes.NewReader(body))
	request.SetPathValue("id", id)
	response = httptest.NewRecorder()
	server.handleMessage(response, request)
	if !strings.Contains(response.Body.String(), "hello:again") {
		t.Fatalf("second response = %s", response.Body.String())
	}
}

func TestHandleMessageSandboxUnavailable(t *testing.T) {
	server := &apiServer{
		config: serverConfig{
			requestLimit: time.Minute,
			dataDir:      t.TempDir(),
			thinking:     "medium",
			model:        "openrouter/free",
			maxSessions:  8,
		},
		sandbox:  Sandbox{Bwrap: filepath.Join(t.TempDir(), "missing"), Pigo: filepath.Join(t.TempDir(), "missing")},
		sessions: map[string]*managedSession{},
	}
	id := createSession(t, server)
	body, _ := json.Marshal(messageRequest{Prompt: "hello"})
	request := httptest.NewRequest(http.MethodPost, "/api/sessions/"+id+"/messages", bytes.NewReader(body))
	request.SetPathValue("id", id)
	response := httptest.NewRecorder()
	server.handleMessage(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
}

func TestHandleFilesRejectsEscape(t *testing.T) {
	server := newTestServer(t)
	id := createSession(t, server)
	request := httptest.NewRequest(http.MethodGet, "/api/sessions/"+id+"/files?path=../etc", nil)
	request.SetPathValue("id", id)
	response := httptest.NewRecorder()
	server.handleListFiles(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
}

func TestHandleListFiles(t *testing.T) {
	server := newTestServer(t)
	id := createSession(t, server)
	managed, _ := server.getSession(id)
	if err := os.WriteFile(filepath.Join(managed.paths.Workspace, "note.txt"), []byte("n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/sessions/"+id+"/files?path=.", nil)
	request.SetPathValue("id", id)
	response := httptest.NewRecorder()
	server.handleListFiles(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "note.txt") {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
}

func TestBwrapHidesHostHomeAndKeys(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not available")
	}
	hostHome, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	probe := filepath.Join(hostHome, ".profile")
	if _, err := os.Stat(probe); err != nil {
		probe = filepath.Join(hostHome, ".bashrc")
		if _, err := os.Stat(probe); err != nil {
			t.Skip("no host home probe file")
		}
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENROUTER_API_KEY", "should-not-leak")
	server := newTestServer(t)
	server.noTools = false
	server.toolNames = builtinToolNames
	server.loopFn = nil
	server.sandbox = Sandbox{Bwrap: bwrap, Pigo: filepath.Join(t.TempDir(), "unused")}
	id := createSession(t, server)
	managed, _ := server.getSession(id)
	managed.mu.Lock()
	defer managed.mu.Unlock()
	if err := server.ensureLive(managed, server.sandboxSpec(managed)); err != nil {
		t.Fatal(err)
	}
	home, err := runIPCJob(context.Background(), managed.paths.Run, "#!/bin/sh\nprintf '%s' \"$HOME\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(home.Stdout) != "/home/pigo" {
		t.Fatalf("HOME = %q", home.Stdout)
	}
	keys, err := runIPCJob(context.Background(), managed.paths.Run, "#!/bin/sh\nprintf '%s' \"$OPENROUTER_API_KEY\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(keys.Stdout, "should-not-leak") {
		t.Fatalf("provider key leaked into sandbox: %q", keys.Stdout)
	}
	stat, err := runIPCJob(context.Background(), managed.paths.Run, "#!/bin/sh\nif [ -e "+shQuote(probe)+" ]; then printf visible; else printf hidden; fi\n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stat.Stdout) != "hidden" {
		t.Fatalf("host home still visible: %q", stat.Stdout)
	}
}

func TestSandboxSpecHasNoProviderKeys(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "secret")
	t.Setenv("ANTHROPIC_API_KEY", "secret")
	server := newTestServer(t)
	id := createSession(t, server)
	managed, _ := server.getSession(id)
	spec := server.sandboxSpec(managed)
	if len(spec.Env) != 0 {
		t.Fatalf("sandbox env = %v, want empty (no provider keys)", spec.Env)
	}
	argv, err := Sandbox{Bwrap: "/usr/bin/bwrap", Pigo: "/bin/true"}.mountArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, "\n")
	if strings.Contains(joined, "secret") || strings.Contains(joined, "API_KEY") {
		t.Fatalf("key material in bwrap argv:\n%s", joined)
	}
}

func newTestServer(t *testing.T) *apiServer {
	t.Helper()
	data := t.TempDir()
	server := &apiServer{
		config: serverConfig{
			dataDir:      data,
			model:        "openrouter/free",
			thinking:     "medium",
			maxSessions:  8,
			requestLimit: time.Minute,
		},
		providerName: "openrouter",
		noTools:      true,
		sessions:     map[string]*managedSession{},
		loopFn: func(_ context.Context, _ *managedSession, prompt string, emit func(streamEvent)) (string, error) {
			text := "hello:" + prompt
			if emit != nil {
				emit(streamEvent{Type: "delta", Text: text})
			}
			return text, nil
		},
	}
	return server
}

func createSession(t *testing.T, server *apiServer) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader("{}"))
	response := httptest.NewRecorder()
	server.handleCreateSession(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create session: %d %s", response.Code, response.Body.String())
	}
	var payload struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ID == "" {
		t.Fatal("missing session id")
	}
	return payload.ID
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/provider"
)

func TestResolveWebInputAction(t *testing.T) {
	meta := &sessionMeta{Model: "openrouter/free", Provider: "openrouter", Thinking: "medium"}
	prompt, message, complete, err := resolveWebInput(meta, "/think low", builtinResolver, presetListing)
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
	prompt, message, complete, err := resolveWebInput(meta, "/fork", builtinResolver, presetListing)
	if err != nil {
		t.Fatalf("resolveWebInput: %v", err)
	}
	if prompt != "" || !complete || !strings.Contains(message, "Web") {
		t.Fatalf("got prompt=%q message=%q complete=%v", prompt, message, complete)
	}
}

func TestResolveWebInputUnknownGoesToPrompt(t *testing.T) {
	meta := &sessionMeta{}
	prompt, _, complete, err := resolveWebInput(meta, "/not-a-command", builtinResolver, presetListing)
	if err != nil {
		t.Fatal(err)
	}
	if complete || prompt != "/not-a-command" {
		t.Fatalf("got prompt=%q complete=%v", prompt, complete)
	}
}

func TestCreateSessionKeepsPreviousHistory(t *testing.T) {
	server := newTestServer(t)
	firstRequest := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader("{}"))
	firstResponse := httptest.NewRecorder()
	server.handleCreateSession(firstResponse, firstRequest)
	var first sessionMeta
	if err := json.Unmarshal(firstResponse.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	materialize(t, server, first.ID)
	newRequest := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader("{}"))
	newResponse := httptest.NewRecorder()
	server.handleCreateSession(newResponse, newRequest)
	if newResponse.Code != http.StatusCreated {
		t.Fatalf("new session: %d %s", newResponse.Code, newResponse.Body.String())
	}
	var created sessionMeta
	if err := json.Unmarshal(newResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == first.ID {
		t.Fatal("new session reused the previous session")
	}
	if len(server.sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(server.sessions))
	}
	if _, err := os.Stat(newSessionPaths(server.config.dataDir, first.ID).Root); err != nil {
		t.Fatalf("previous session history was removed: %v", err)
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
			turnMax:     time.Minute,
			dataDir:     t.TempDir(),
			thinking:    "medium",
			model:       "openrouter/free",
			maxSessions: 8,
		},
		sandbox:  Sandbox{Bwrap: filepath.Join(t.TempDir(), "missing"), Pigo: filepath.Join(t.TempDir(), "missing")},
		db:       openTestDB(t),
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

func TestHandleModelsLoadsLiveOpenRouterFreeTextModels(t *testing.T) {
	clearProviderKeys(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("models request must not send provider credentials")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[
			{"id":"nvidia/nemotron-3-ultra:free","name":"Nemotron Ultra (free)","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]}},
			{"id":"paid/model","name":"Paid","pricing":{"prompt":"0.1","completion":"0"},"architecture":{"output_modalities":["text"]}},
			{"id":"free/embed","name":"Embedding","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["embeddings"]}},
			{"id":"free/audio","name":"Audio generator","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text","audio"]}},
			{"id":"nvidia/content-safety:free","name":"Content Safety","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]}}
		]}`)
	}))
	defer upstream.Close()

	server := openRouterKeyedServer(t, upstream)
	request := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	response := httptest.NewRecorder()
	server.handleModels(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
	var models []modelResponse
	if err := json.Unmarshal(response.Body.Bytes(), &models); err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "nvidia/nemotron-3-ultra:free" || models[0].Provider != "openrouter" {
		t.Fatalf("models = %+v", models)
	}
}

// openRouterKeyedServer is a test server with a stored OpenRouter key whose
// free catalog comes from upstream.
func openRouterKeyedServer(t *testing.T, upstream *httptest.Server) *apiServer {
	t.Helper()
	server := newTestServer(t)
	server.modelHTTPClient = upstream.Client()
	server.openRouterModelsURL = upstream.URL
	if err := server.credentials.setPublicKey("openrouter", "sk-or"); err != nil {
		t.Fatal(err)
	}
	return server
}

// TestHandleModelsNeedsOpenRouterKey checks the free catalog is only offered
// with a stored OpenRouter key — a key in the environment does not count — and
// that an empty list is [], not null.
func TestHandleModelsNeedsOpenRouterKey(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "env-key")
	fetched := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetched = true
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer upstream.Close()
	server := newTestServer(t)
	server.modelHTTPClient = upstream.Client()
	server.openRouterModelsURL = upstream.URL
	response := httptest.NewRecorder()
	server.handleModels(response, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "[]" || fetched {
		t.Fatalf("status = %d body = %s fetched = %v", response.Code, response.Body.String(), fetched)
	}
}

func TestHandleModelsReportsOpenRouterFailure(t *testing.T) {
	clearProviderKeys(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	server := openRouterKeyedServer(t, upstream)
	request := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	response := httptest.NewRecorder()
	server.handleModels(response, request)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "OpenRouter returned 503") {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
}

func TestUpdateSessionKeepsExplicitOpenRouterProvider(t *testing.T) {
	server := newTestServer(t)
	id := createSession(t, server)
	body := strings.NewReader(`{"model":"nvidia/nemotron-3-ultra-550b-a55b:free","provider":"openrouter","thinking":"medium"}`)
	request := httptest.NewRequest(http.MethodPatch, "/api/sessions/"+id, body)
	request.SetPathValue("id", id)
	response := httptest.NewRecorder()
	server.handleUpdateSession(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
	managed, _ := server.getSession(id)
	if managed.meta.Provider != "openrouter" {
		t.Fatalf("provider = %q, want openrouter", managed.meta.Provider)
	}
}

func TestSessionResponseUsesEmptyToolsArray(t *testing.T) {
	server := newTestServer(t)
	id := createSession(t, server)
	managed, _ := server.getSession(id)
	response := sessionResponse(managed)
	tools, ok := response["tools"].([]string)
	if !ok || tools == nil || len(tools) != 0 {
		t.Fatalf("tools = %#v, want non-nil empty []string", response["tools"])
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
	home, err := runLiveJob(context.Background(), managed.live, "printf '%s' \"$HOME\"")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(home.Stdout) != "/home/pigo" {
		t.Fatalf("HOME = %q", home.Stdout)
	}
	keys, err := runLiveJob(context.Background(), managed.live, "printf '%s' \"$OPENROUTER_API_KEY\"")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(keys.Stdout, "should-not-leak") {
		t.Fatalf("provider key leaked into sandbox: %q", keys.Stdout)
	}
	stat, err := runLiveJob(context.Background(), managed.live, "if [ -e "+shQuote(probe)+" ]; then printf visible; else printf hidden; fi")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stat.Stdout) != "hidden" {
		t.Fatalf("host home still visible: %q", stat.Stdout)
	}
	if _, err := runLiveJob(context.Background(), managed.live, "touch /tmp/pigo-live-probe"); err != nil {
		t.Fatal(err)
	}
	tmpState, err := runLiveJob(context.Background(), managed.live, "test -f /tmp/pigo-live-probe && printf persisted")
	if err != nil {
		t.Fatal(err)
	}
	if tmpState.Stdout != "persisted" {
		t.Fatalf("sandbox /tmp did not persist between commands: %q", tmpState.Stdout)
	}

	previous := managed.live
	cancelCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, cancelErr := runLiveJob(cancelCtx, previous, "sleep 30")
	cancel()
	if cancelErr != context.DeadlineExceeded {
		t.Fatalf("cancelled bwrap command error = %v, want deadline exceeded", cancelErr)
	}
	if previous.alive() {
		t.Fatal("cancelled bwrap remained eligible for reuse")
	}
	if err := server.ensureLive(managed, server.sandboxSpec(managed)); err != nil {
		t.Fatalf("restart sandbox after cancellation: %v", err)
	}
	if managed.live == previous {
		t.Fatal("cancelled bwrap was reused")
	}
	recovered, err := runLiveJob(context.Background(), managed.live, "printf recovered")
	if err != nil || recovered.Stdout != "recovered" {
		t.Fatalf("run after sandbox restart: result=%+v err=%v", recovered, err)
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

// clearProviderKeys blanks every provider key variable for the test, so model
// listings do not change with whatever keys the machine running it happens to
// hold.
func clearProviderKeys(t *testing.T) {
	t.Helper()
	for _, spec := range provider.ProviderSpecs() {
		for _, env := range spec.EnvVars {
			t.Setenv(env, "")
		}
	}
}

// builtinResolver is the registry-only resolver, for tests that exercise slash
// commands without a settings store.
func builtinResolver(model, providerName string) (provider.Provider, string, error) {
	return provider.ResolveProvider(model, "", "", providerName, os.Getenv)
}

func newTestServer(t *testing.T) *apiServer {
	t.Helper()
	data := t.TempDir()
	db := openTestDB(t)
	auth, err := newAuthStore(db)
	if err != nil {
		t.Fatal(err)
	}
	cfg := serverConfig{
		dataDir:     data,
		model:       "openrouter/free",
		thinking:    "medium",
		maxSessions: 8,
		turnMax:     time.Minute,
	}
	// The credential store is enabled in tests so handlers exercise the real
	// encrypt/decrypt path rather than the disabled fallback.
	credentials, err := newCredentialStore(db, "test-master-secret")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := newSettingsStore(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	customModels, err := newCustomModelStore(db)
	if err != nil {
		t.Fatal(err)
	}
	server := &apiServer{
		config:       cfg,
		credentials:  credentials,
		settings:     settings,
		customModels: customModels,
		providerName: "openrouter",
		noTools:      true,
		auth:         auth,
		db:           db,
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
	// A new session is a draft until its first message; most tests want a
	// stored one, as if a message had been sent.
	materialize(t, server, payload.ID)
	return payload.ID
}

func materialize(t *testing.T, server *apiServer, id string) {
	t.Helper()
	managed := server.sessions[id]
	managed.mu.Lock()
	defer managed.mu.Unlock()
	if err := server.materializeLocked(managed); err != nil {
		t.Fatal(err)
	}
}

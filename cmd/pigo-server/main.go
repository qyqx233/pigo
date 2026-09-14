// Command pigo-server is an HTTP orchestrator: the agent loop and provider
// credentials stay in this process. File tools run against the session
// workspace on the host; bash runs inside a long-lived bwrap with no API keys.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	serverweb "github.com/smallnest/pigo/cmd/pigo-server/web"
	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/provider"
	"github.com/smallnest/pigo/internal/runtime"
)

const maxRequestBody = 1 << 20 // 1 MiB

type serverConfig struct {
	listen       string
	dataDir      string
	model        string
	provider     string
	thinking     string
	tools        string
	skills       bool
	token        string
	maxSessions  int
	requestLimit time.Duration
	idleTimeout  time.Duration
}

type apiServer struct {
	config       serverConfig
	sandbox      Sandbox
	loopFn       func(context.Context, *managedSession, string, func(streamEvent)) (string, error)
	providerName string
	noTools      bool
	toolNames    []string
	reaperStop   chan struct{}

	mu       sync.RWMutex
	sessions map[string]*managedSession
}

type managedSession struct {
	mu       sync.Mutex
	paths    sessionPaths
	meta     sessionMeta
	live     *exec.Cmd
	busy     bool
	closed   bool
	agentCtx *agentcore.AgentContext
	runCfg   runtime.RunConfig
}

type messageRequest struct {
	Prompt string `json:"prompt"`
}

type sessionUpdateRequest struct {
	Model    string `json:"model"`
	Thinking string `json:"thinking"`
}

type modelResponse struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Provider string `json:"provider"`
}

type streamEvent struct {
	Type    string `json:"type"`
	Text    string `json:"text,omitempty"`
	Error   string `json:"error,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Phase   string `json:"phase,omitempty"`
	ID      string `json:"id,omitempty"`
	IsError bool   `json:"isError,omitempty"`
}

func main() {
	cfg := serverConfig{token: os.Getenv("PIGO_SERVER_TOKEN")}
	flag.StringVar(&cfg.listen, "listen", "127.0.0.1:8080", "HTTP listen address")
	flag.StringVar(&cfg.dataDir, "data", defaultDataDir(), "session workspace root")
	flag.StringVar(&cfg.model, "model", "openrouter/free", "model id")
	flag.StringVar(&cfg.provider, "provider", "", "optional provider override")
	flag.StringVar(&cfg.thinking, "thinking", "medium", "reasoning effort")
	flag.StringVar(&cfg.tools, "tools", "", "comma-separated tool allowlist; empty disables tools, 'all' enables all tools")
	flag.BoolVar(&cfg.skills, "skills", false, "bind the host skills directory into the sandbox (off by default)")
	flag.IntVar(&cfg.maxSessions, "max-sessions", 32, "maximum live browser sessions")
	flag.DurationVar(&cfg.requestLimit, "request-timeout", 10*time.Minute, "maximum duration of one agent request")
	flag.DurationVar(&cfg.idleTimeout, "idle", 30*time.Minute, "stop an idle sandbox process after this duration; 0 disables idle expiry. Disk state is kept. There is no maximum lifetime.")
	flag.Parse()

	if cfg.maxSessions < 1 {
		log.Fatal("-max-sessions must be at least 1")
	}
	if cfg.requestLimit <= 0 {
		log.Fatal("-request-timeout must be positive")
	}
	if !isLoopbackAddress(cfg.listen) && cfg.token == "" {
		log.Fatal("PIGO_SERVER_TOKEN is required when -listen is not a loopback address")
	}

	api, err := newAPIServer(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer api.close()

	mux := http.NewServeMux()
	mux.Handle("GET /", serverweb.Handler())
	mux.HandleFunc("GET /healthz", api.handleHealth)
	mux.Handle("POST /api/sessions", api.requireAuth(http.HandlerFunc(api.handleCreateSession)))
	mux.Handle("GET /api/sessions/{id}", api.requireAuth(http.HandlerFunc(api.handleGetSession)))
	mux.Handle("PATCH /api/sessions/{id}", api.requireAuth(http.HandlerFunc(api.handleUpdateSession)))
	mux.Handle("DELETE /api/sessions/{id}", api.requireAuth(http.HandlerFunc(api.handleDeleteSession)))
	mux.Handle("GET /api/sessions/{id}/commands", api.requireAuth(http.HandlerFunc(api.handleCommands)))
	mux.Handle("POST /api/sessions/{id}/messages", api.requireAuth(http.HandlerFunc(api.handleMessage)))
	mux.Handle("GET /api/sessions/{id}/files", api.requireAuth(http.HandlerFunc(api.handleListFiles)))
	mux.Handle("GET /api/sessions/{id}/files/raw", api.requireAuth(http.HandlerFunc(api.handleReadFile)))
	mux.Handle("GET /api/models", api.requireAuth(http.HandlerFunc(api.handleModels)))

	httpServer := &http.Server{
		Addr:              cfg.listen,
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		log.Printf("pigo web server listening on http://%s (model=%s, tools=%s, sandbox=bwrap, data=%s)",
			cfg.listen, cfg.model, toolSummary(cfg.tools), cfg.dataDir)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP shutdown: %v", err)
		}
	}
}

func newAPIServer(cfg serverConfig) (*apiServer, error) {
	if strings.TrimSpace(cfg.dataDir) == "" {
		cfg.dataDir = defaultDataDir()
	}
	if err := os.MkdirAll(filepath.Join(cfg.dataDir, "sessions"), 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	if !validThinking(cfg.thinking) {
		return nil, fmt.Errorf("invalid thinking level %q", cfg.thinking)
	}
	_, providerName, err := provider.ResolveProvider(cfg.model, "", "", cfg.provider, os.Getenv)
	if err != nil {
		return nil, fmt.Errorf("configure agent: %w", err)
	}
	bwrap, bwrapErr := findBwrap()
	sandbox := Sandbox{Bwrap: bwrap}
	if bwrapErr != nil {
		log.Printf("pigo-server: sandbox unavailable: %v (bash tools will fail closed)", bwrapErr)
	}
	noTools, toolNames := configuredTools(cfg.tools)
	sessions := loadSessions(cfg.dataDir)
	s := &apiServer{
		config:       cfg,
		sandbox:      sandbox,
		providerName: providerName,
		noTools:      noTools,
		toolNames:    toolNames,
		reaperStop:   make(chan struct{}),
		sessions:     sessions,
	}
	if cfg.idleTimeout > 0 {
		go s.reapLoop()
	}
	return s, nil
}

func (s *apiServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	count := len(s.sessions)
	s.mu.RUnlock()
	status := "ok"
	sandboxErr := ""
	if err := s.sandbox.Ready(); err != nil && s.loopFn == nil && s.needsSandbox() {
		status = "degraded"
		sandboxErr = err.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   status,
		"sessions": count,
		"idle":     idleLabel(s.config.idleTimeout),
		"sandbox": map[string]any{
			"kind":  "bwrap",
			"mode":  "keepalive",
			"ready": sandboxErr == "",
			"error": sandboxErr,
			"bwrap": s.sandbox.Bwrap,
			"pigo":  s.sandbox.Pigo,
		},
	})
}

func (s *apiServer) handleModels(w http.ResponseWriter, _ *http.Request) {
	models := make([]modelResponse, 0, len(provider.PresetCatalog))
	for _, model := range provider.PresetCatalog {
		models = append(models, modelResponse{
			ID:       model.ID,
			Label:    model.Label(),
			Provider: model.Provider,
		})
	}
	writeJSON(w, http.StatusOK, models)
}

func (s *apiServer) handleCommands(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.getSession(r.PathValue("id")); !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, webCommands())
}

func (s *apiServer) handleCreateSession(w http.ResponseWriter, _ *http.Request) {
	id, err := randomID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create session id")
		return
	}
	now := time.Now().UTC()
	paths := newSessionPaths(s.config.dataDir, id)
	if err := paths.create(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	managed := &managedSession{
		paths: paths,
		meta: sessionMeta{
			ID:        id,
			Model:     s.config.model,
			Provider:  s.providerName,
			Thinking:  s.config.thinking,
			Tools:     append([]string(nil), s.toolNames...),
			CreatedAt: now,
			LastUsed:  now,
		},
	}
	if err := paths.saveMeta(managed.meta); err != nil {
		_ = paths.remove()
		writeError(w, http.StatusInternalServerError, "write session metadata")
		return
	}

	s.mu.Lock()
	if len(s.sessions) >= s.config.maxSessions {
		s.mu.Unlock()
		_ = paths.remove()
		writeError(w, http.StatusServiceUnavailable, "session limit reached")
		return
	}
	s.sessions[id] = managed
	s.mu.Unlock()

	writeJSON(w, http.StatusCreated, sessionResponse(managed))
}

func (s *apiServer) handleGetSession(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.getSession(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	managed.mu.Lock()
	defer managed.mu.Unlock()
	if managed.closed {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse(managed))
}

func (s *apiServer) handleUpdateSession(w http.ResponseWriter, r *http.Request) {
	var request sessionUpdateRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(request.Model) == "" && strings.TrimSpace(request.Thinking) == "" {
		writeError(w, http.StatusBadRequest, "model or thinking is required")
		return
	}

	managed, ok := s.getSession(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	managed.mu.Lock()
	defer managed.mu.Unlock()
	if managed.closed {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if model := strings.TrimSpace(request.Model); model != "" {
		if _, err := applyModel(&managed.meta, model); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if thinking := strings.TrimSpace(request.Thinking); thinking != "" {
		if _, err := applyThinking(&managed.meta, thinking); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	managed.meta.LastUsed = time.Now().UTC()
	if err := s.applyHostConfig(managed); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := managed.paths.saveMeta(managed.meta); err != nil {
		writeError(w, http.StatusInternalServerError, "write session metadata")
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse(managed))
}

func (s *apiServer) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	managed, ok := s.sessions[id]
	if ok {
		delete(s.sessions, id)
	}
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	managed.mu.Lock()
	managed.closed = true
	managed.stopLive()
	paths := managed.paths
	managed.mu.Unlock()
	if err := paths.remove(); err != nil {
		writeError(w, http.StatusInternalServerError, "remove session: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *apiServer) handleMessage(w http.ResponseWriter, r *http.Request) {
	var request messageRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.Prompt = strings.TrimSpace(request.Prompt)
	if request.Prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}

	id := r.PathValue("id")
	managed, ok := s.getSession(id)
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	managed.mu.Lock()
	defer managed.mu.Unlock()
	if managed.closed {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	managed.meta.LastUsed = time.Now().UTC()
	prompt, prefix, complete, err := resolveWebInput(&managed.meta, request.Prompt)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = managed.paths.saveMeta(managed.meta)

	ctx, cancel := context.WithTimeout(r.Context(), s.config.requestLimit)
	defer cancel()
	if r.URL.Query().Get("stream") == "true" {
		if complete {
			writeStreamText(w, prefix)
			return
		}
		s.streamMessage(ctx, w, id, managed, prompt, prefix)
		return
	}

	if complete {
		writeJSON(w, http.StatusOK, map[string]string{"reply": prefix})
		return
	}
	reply, runErr := s.runMessage(ctx, managed, prompt)
	if runErr != nil {
		log.Printf("pigo-server: session %s: agent request failed: %v", shortID(id), runErr)
		writeError(w, statusForError(runErr), runErr.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"reply": prefixText(prefix, reply)})
}

func (s *apiServer) streamMessage(ctx context.Context, w http.ResponseWriter, id string, managed *managedSession, prompt, prefix string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	encoder := json.NewEncoder(w)
	var writeErr error
	emit := func(event streamEvent) {
		if writeErr != nil {
			return
		}
		if err := encoder.Encode(event); err != nil {
			writeErr = err
			return
		}
		flusher.Flush()
	}

	if prefix != "" {
		emit(streamEvent{Type: "delta", Text: prefix + "\n\n"})
	}
	reply, err := s.runMessage(ctx, managed, prompt, emit)
	if writeErr != nil {
		return
	}
	if err != nil {
		log.Printf("pigo-server: session %s: streaming agent request failed: %v", shortID(id), err)
		emit(streamEvent{Type: "error", Error: err.Error()})
		return
	}
	emit(streamEvent{Type: "done", Text: prefixText(prefix, reply)})
}

func (s *apiServer) runMessage(ctx context.Context, managed *managedSession, prompt string, emit ...func(streamEvent)) (string, error) {
	var emitFn func(streamEvent)
	if len(emit) > 0 {
		emitFn = emit[0]
	}
	if s.loopFn != nil {
		return s.loopFn(ctx, managed, prompt, emitFn)
	}
	if s.needsSandbox() {
		if err := s.sandbox.Ready(); err != nil {
			return "", fmt.Errorf("sandbox unavailable: %w", err)
		}
	}
	managed.busy = true
	defer func() { managed.busy = false }()
	return s.runHostLoop(ctx, managed, prompt, emitFn)
}

func (s *apiServer) needsSandbox() bool {
	if s.noTools {
		return false
	}
	names := s.toolNames
	if len(names) == 0 {
		names = builtinToolNames
	}
	for _, name := range names {
		if name == "bash" {
			return true
		}
	}
	return false
}

func toolsAreAll(value string) bool {
	items := splitList(value)
	return len(items) == 1 && strings.EqualFold(items[0], "all")
}

func (s *apiServer) handleListFiles(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.getSession(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	managed.mu.Lock()
	defer managed.mu.Unlock()
	if managed.closed {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	rel := r.URL.Query().Get("path")
	entries, err := listWorkspace(managed.paths.Workspace, rel)
	if err != nil {
		if errors.Is(err, errPathEscape) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": rel, "entries": entries})
}

func (s *apiServer) handleReadFile(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.getSession(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	managed.mu.Lock()
	defer managed.mu.Unlock()
	if managed.closed {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	rel := r.URL.Query().Get("path")
	data, err := readWorkspaceFile(managed.paths.Workspace, rel)
	if err != nil {
		if errors.Is(err, errPathEscape) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filepath.Base(rel)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func writeStreamText(w http.ResponseWriter, text string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(streamEvent{Type: "done", Text: text})
	flusher.Flush()
}

func prefixText(prefix, text string) string {
	if prefix == "" {
		return text
	}
	if text == "" {
		return prefix
	}
	return prefix + "\n\n" + text
}

func (s *apiServer) getSession(id string) (*managedSession, bool) {
	s.mu.RLock()
	managed, ok := s.sessions[id]
	s.mu.RUnlock()
	return managed, ok
}

func (s *apiServer) close() {
	if s.reaperStop != nil {
		select {
		case <-s.reaperStop:
		default:
			close(s.reaperStop)
		}
	}
	s.mu.Lock()
	sessions := make([]*managedSession, 0, len(s.sessions))
	for _, managed := range s.sessions {
		sessions = append(sessions, managed)
	}
	s.mu.Unlock()
	for _, managed := range sessions {
		managed.mu.Lock()
		managed.closed = true
		managed.stopLive()
		managed.mu.Unlock()
	}
}

func (s *apiServer) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.config.token != "" {
			provided, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok || subtle.ConstantTimeCompare([]byte(provided), []byte(s.config.token)) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="pigo-server"`)
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func sessionResponse(managed *managedSession) map[string]any {
	m := managed.meta
	return map[string]any{
		"id":            m.ID,
		"model":         m.Model,
		"provider":      m.Provider,
		"thinking":      m.Thinking,
		"tools":         m.Tools,
		"createdAt":     m.CreatedAt,
		"lastUsed":      m.LastUsed,
		"pigoSessionId": m.PigoSessionID,
		"sandbox":       "bwrap",
		"alive":         managed.liveAlive(),
	}
}

func statusForError(err error) int {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	msg := err.Error()
	if strings.Contains(msg, "sandbox unavailable") {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadGateway
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func randomID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

func splitList(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func idleLabel(d time.Duration) string {
	if d <= 0 {
		return "off"
	}
	return d.String()
}

func toolSummary(value string) string {
	if strings.TrimSpace(value) == "" {
		return "disabled"
	}
	return value
}

func isLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Command pigo-server is an HTTP orchestrator: the agent loop and provider
// credentials stay in this process. File tools run against the session
// workspace on the host; bash runs inside a long-lived bwrap with no API keys.
package main

import (
	"context"
	"crypto/rand"
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
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/smallnest/pigo/cmd/pigo-server/ext/all"
	serverweb "github.com/smallnest/pigo/cmd/pigo-server/web"
	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/runtime"
)

const maxRequestBody = 1 << 20 // 1 MiB

type serverConfig struct {
	listen  string
	dataDir string
	// dbURL is the -db value: sqlite:<path> or postgres://…; empty means
	// <dataDir>/pigo.db.
	dbURL       string
	model       string
	provider    string
	thinking    string
	tools       string
	skills      bool
	token       string
	admins      adminRoster
	maxSessions int
	idleTimeout time.Duration
	// turnIdle, turnMax and turnMaxSteps are a turn's brakes (turnrun.go).
	turnIdle     time.Duration
	turnMax      time.Duration
	turnMaxSteps int
	// sandboxTools are the -sandbox-tool / PIGO_SANDBOX_TOOLS entries
	// (name=dir), mounted read-only at /opt/<name> in every sandbox.
	sandboxTools []string
	// toolsConfig is the -tools-config / PIGO_TOOLS_CONFIG file: extension
	// tools and naming profiles (tools_config.go).
	toolsConfig string
	// turnTick is how often the brakes are checked; zero means the default.
	// Not a flag: tests shorten it.
	turnTick        time.Duration
	emptySessionTTL time.Duration
}

type apiServer struct {
	config              serverConfig
	sandbox             Sandbox
	loopFn              func(context.Context, *managedSession, string, func(streamEvent)) (string, error)
	modelHTTPClient     *http.Client
	openRouterModelsURL string
	providerName        string
	noTools             bool
	toolNames           []string
	exts                *toolExtensions
	reaperStop          chan struct{}
	auth                *authStore
	customModels        *customModelStore
	credentials         *credentialStore
	settings            *settingsStore
	ledger              *ledgerStore
	meter               *meter
	db                  *sqlDB

	mu       sync.RWMutex
	sessions map[string]*managedSession
}

type managedSession struct {
	mu    sync.Mutex
	paths sessionPaths
	meta  sessionMeta
	live  *liveSandbox
	// turn is the running turn, or the last one for a while after it ends
	// (see turnRetention). activeTurn reports only a running one.
	turn   *turnRun
	closed bool
	// transcript tracks what of the agent's messages is stored.
	transcript transcriptState
	agentCtx   *agentcore.AgentContext
	runCfg     runtime.RunConfig
}

type messageRequest struct {
	Prompt string `json:"prompt"`
}

type sessionUpdateRequest struct {
	Model    string `json:"model"`
	Provider string `json:"provider,omitempty"`
	Thinking string `json:"thinking"`
}

type modelResponse struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Provider string `json:"provider"`
	// Source groups the entry for pickers: "custom" (added on this server),
	// "free" (OpenRouter's live free catalog) or "preset" (the built-in preset
	// of a provider that has a key).
	Source string `json:"source,omitempty"`
}

type streamEvent struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	Error string `json:"error,omitempty"`
	Tool  string `json:"tool,omitempty"`
	// Notice names a transient status the client may surface while the run
	// continues (currently only "retry"). A client that does not know the
	// notice type can ignore the event: it carries no conversation content.
	Notice  string `json:"notice,omitempty"`
	Phase   string `json:"phase,omitempty"`
	ID      string `json:"id,omitempty"`
	IsError bool   `json:"isError,omitempty"`
	// Usage is the "usage" event's payload: one model call's tokens and cost.
	Usage *usageReport `json:"usage,omitempty"`

	// The fields below belong to turn streams (turnrun.go).
	//
	// Usages is the "snapshot" event's list of the turn's calls so far.
	Usages []*usageReport `json:"usages,omitempty"`
	// Steps counts the turn's tool calls so far.
	Steps     int        `json:"steps,omitempty"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	// ElapsedMs is how long the "heartbeat" event's phase has lasted.
	ElapsedMs int64 `json:"elapsedMs,omitempty"`
	// Reason is why the turn ended ("done", "stalled", …), on its final event.
	Reason string `json:"reason,omitempty"`
	// Detail explains a turn that finished but not simply (the step limit),
	// or, on a tool's start, what the call is doing (activity.go).
	Detail string `json:"detail,omitempty"`
	// Activity is the "snapshot" event's log of the turn so far.
	Activity []activityItem `json:"activity,omitempty"`
}

func main() {
	// "pigo-server migrate" moves the state of an older release, kept in JSON
	// files, into the database.
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		os.Exit(runMigrate(os.Args[2:]))
	}
	cfg := serverConfig{
		token:  os.Getenv("PIGO_SERVER_TOKEN"),
		admins: parseAdminRoster(os.Getenv("PIGO_ADMIN_USERS")),
	}
	flag.StringVar(&cfg.listen, "listen", "127.0.0.1:8080", "HTTP listen address")
	flag.StringVar(&cfg.dataDir, "data", defaultDataDir(), "data directory: session workspaces, and the SQLite database by default")
	flag.StringVar(&cfg.dbURL, "db", os.Getenv("PIGO_DB"), "database: sqlite:<path> (default <data>/pigo.db) or postgres://user:pass@host/db; also PIGO_DB")
	flag.StringVar(&cfg.model, "model", "openrouter/free", "model id")
	flag.StringVar(&cfg.provider, "provider", "", "optional provider override")
	flag.StringVar(&cfg.thinking, "thinking", "medium", "reasoning effort")
	flag.StringVar(&cfg.tools, "tools", "", "comma-separated tool allowlist; empty disables tools, 'all' enables all tools")
	flag.BoolVar(&cfg.skills, "skills", false, "bind the host skills directory into the sandbox (off by default)")
	var tools toolFlag
	flag.Var(&tools, "sandbox-tool", "mount a self-contained toolchain read-only at /opt/<name> in every sandbox, its bin/ first on PATH: name=dir (repeatable; also PIGO_SANDBOX_TOOLS, comma-separated). Build a Python one with cmd/pigo-server/sandbox-python/build.sh")
	flag.StringVar(&cfg.toolsConfig, "tools-config", os.Getenv("PIGO_TOOLS_CONFIG"), "YAML file of extension tools (added, or replacing a built-in of the same name) and per-model tool naming; also PIGO_TOOLS_CONFIG. See spec/tool-extensions.md")
	flag.IntVar(&cfg.maxSessions, "max-sessions", 32, "maximum live browser sessions")
	flag.DurationVar(&cfg.turnIdle, "turn-idle", 10*time.Minute, "stop a turn that makes no progress (no model output, no tool starting or finishing) for this long; 0 disables")
	flag.DurationVar(&cfg.turnMax, "turn-max", 2*time.Hour, "stop a turn that runs longer than this in total; 0 disables")
	flag.IntVar(&cfg.turnMaxSteps, "turn-max-steps", 200, "ask the model to wrap up after this many tool calls in one turn; 0 disables")
	var requestTimeout time.Duration
	flag.DurationVar(&requestTimeout, "request-timeout", 0, "deprecated: use -turn-max")
	flag.DurationVar(&cfg.idleTimeout, "idle", 30*time.Minute, "stop an idle sandbox process after this duration; 0 disables idle expiry. Disk state is kept. There is no maximum lifetime.")
	flag.DurationVar(&cfg.emptySessionTTL, "empty-session-ttl", time.Hour, "delete sessions with no transcript and an empty workspace after this duration; 0 disables cleanup")
	flag.Parse()
	cfg.sandboxTools = toolSpecs(os.Getenv("PIGO_SANDBOX_TOOLS"), tools)

	if cfg.maxSessions < 1 {
		log.Fatal("-max-sessions must be at least 1")
	}
	if requestTimeout > 0 {
		log.Printf("pigo-server: -request-timeout is deprecated; it now sets -turn-max (a turn no longer ends with the request that started it)")
		cfg.turnMax = requestTimeout
	}
	if cfg.turnIdle < 0 || cfg.turnMax < 0 || cfg.turnMaxSteps < 0 {
		log.Fatal("-turn-idle, -turn-max and -turn-max-steps cannot be negative")
	}
	if cfg.emptySessionTTL < 0 {
		log.Fatal("-empty-session-ttl cannot be negative")
	}

	api, err := newAPIServer(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer api.close()

	mux := http.NewServeMux()
	mux.Handle("GET /", serverweb.Handler())
	mux.HandleFunc("GET /healthz", api.handleHealth)
	mux.HandleFunc("POST /api/auth/register", api.handleRegister)
	mux.HandleFunc("POST /api/auth/login", api.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", api.handleLogout)
	mux.Handle("GET /api/auth/me", api.requirePrincipal(http.HandlerFunc(api.handleMe)))
	mux.Handle("GET /api/sessions", api.requirePrincipal(http.HandlerFunc(api.handleListSessions)))
	mux.Handle("POST /api/sessions", api.requirePrincipal(http.HandlerFunc(api.handleCreateSession)))
	mux.Handle("GET /api/sessions/{id}", api.requirePrincipal(http.HandlerFunc(api.handleGetSession)))
	mux.Handle("PATCH /api/sessions/{id}", api.requirePrincipal(http.HandlerFunc(api.handleUpdateSession)))
	mux.Handle("DELETE /api/sessions/{id}", api.requirePrincipal(http.HandlerFunc(api.handleDeleteSession)))
	mux.Handle("GET /api/sessions/{id}/commands", api.requirePrincipal(http.HandlerFunc(api.handleCommands)))
	mux.Handle("GET /api/sessions/{id}/messages", api.requirePrincipal(http.HandlerFunc(api.handleSessionMessages)))
	mux.Handle("POST /api/sessions/{id}/messages", api.requirePrincipal(http.HandlerFunc(api.handleMessage)))
	mux.Handle("GET /api/sessions/{id}/turn", api.requirePrincipal(http.HandlerFunc(api.handleAttachTurn)))
	mux.Handle("POST /api/sessions/{id}/turn/cancel", api.requirePrincipal(http.HandlerFunc(api.handleCancelTurn)))
	mux.Handle("GET /api/sessions/{id}/usage", api.requirePrincipal(http.HandlerFunc(api.handleSessionUsage)))
	mux.Handle("GET /api/sessions/{id}/files", api.requirePrincipal(http.HandlerFunc(api.handleListFiles)))
	mux.Handle("GET /api/sessions/{id}/files/raw", api.requirePrincipal(http.HandlerFunc(api.handleReadFile)))
	mux.Handle("GET /api/models", api.requirePrincipal(http.HandlerFunc(api.handleModels)))
	mux.Handle("GET /api/custom-models", api.requirePrincipal(http.HandlerFunc(api.handleListCustomModels)))
	mux.Handle("GET /api/providers", api.requirePrincipal(http.HandlerFunc(api.handleListProviders)))
	mux.Handle("POST /api/custom-models", api.requirePrincipal(http.HandlerFunc(api.handleAddCustomModel)))
	mux.Handle("DELETE /api/custom-models/{id...}", api.requirePrincipal(http.HandlerFunc(api.handleDeleteCustomModel)))

	// Billing: the price table is readable by everyone; a user sees their own
	// usage.
	mux.Handle("GET /api/prices", api.requirePrincipal(http.HandlerFunc(api.handleListPrices)))
	mux.Handle("GET /api/usage", api.requirePrincipal(http.HandlerFunc(api.handleMyUsage)))

	// A user's own provider keys.
	mux.Handle("GET /api/credentials", api.requirePrincipal(http.HandlerFunc(api.handleListCredentials)))
	mux.Handle("PUT /api/credentials/{provider}", api.requirePrincipal(http.HandlerFunc(api.handleSetCredential)))
	mux.Handle("DELETE /api/credentials/{provider}", api.requirePrincipal(http.HandlerFunc(api.handleDeleteCredential)))

	// The admin console. requireAdmin runs inside requirePrincipal, so an
	// anonymous request is rejected as unauthorized before the role is checked.
	admin := func(h http.HandlerFunc) http.Handler {
		return api.requirePrincipal(api.requireAdmin(h))
	}
	mux.Handle("GET /api/admin/settings", admin(api.handleGetSettings))
	mux.Handle("PUT /api/admin/settings", admin(api.handleUpdateSettings))
	mux.Handle("GET /api/admin/credentials", admin(api.handleListPublicCredentials))
	mux.Handle("PUT /api/admin/credentials/{provider}", admin(api.handleSetPublicCredential))
	mux.Handle("DELETE /api/admin/credentials/{provider}", admin(api.handleDeletePublicCredential))
	mux.Handle("GET /api/admin/models", admin(api.handleListPublicModels))
	mux.Handle("POST /api/admin/models", admin(api.handleAddPublicModel))
	mux.Handle("DELETE /api/admin/models/{id...}", admin(api.handleDeletePublicModel))
	mux.Handle("GET /api/admin/providers", admin(api.handleListCustomProviders))
	mux.Handle("PUT /api/admin/providers", admin(api.handlePutCustomProvider))
	mux.Handle("DELETE /api/admin/providers/{name}", admin(api.handleDeleteCustomProvider))
	mux.Handle("PATCH /api/admin/providers/{name}", admin(api.handlePatchCustomProvider))
	mux.Handle("PUT /api/admin/prices", admin(api.handlePutPrice))
	mux.Handle("DELETE /api/admin/prices", admin(api.handleDeletePrice))
	mux.Handle("GET /api/admin/usage", admin(api.handleAdminUsage))
	mux.Handle("GET /api/admin/users", admin(api.handleListUsers))
	mux.Handle("POST /api/admin/users/{id}/disable", admin(api.handleSetUserDisabled))
	mux.Handle("POST /api/admin/users/{id}/password", admin(api.handleResetUserPassword))
	mux.Handle("DELETE /api/admin/users/{id}", admin(api.handleDeleteUser))

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
		log.Printf("admins: %s", cfg.admins.describe())
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	case <-ctx.Done():
		// Turns outlive requests, so they are stopped first: each saves what
		// it has and tells its clients, then the connections can drain.
		api.stopAllTurns(8 * time.Second)
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
	// State left in the old JSON files would be silently ignored: refuse to
	// start until it has been moved into the database.
	if files := findLegacyFiles(cfg.dataDir); len(files) > 0 {
		return nil, legacyFilesError(cfg.dataDir, files)
	}
	target, err := parseDBTarget(cfg.dbURL, cfg.dataDir)
	if err != nil {
		return nil, err
	}
	db, err := openDB(target)
	if err != nil {
		return nil, err
	}
	log.Printf("pigo-server: database %s", target)
	opened := false
	defer func() {
		if !opened {
			db.Close()
		}
	}()
	// Settings first: the default model may name a custom endpoint, which only
	// the settings store knows about.
	settings, err := newSettingsStore(db, cfg)
	if err != nil {
		return nil, err
	}
	_, providerName, err := resolveProviderWith(settings, cfg.model, cfg.provider)
	if err != nil {
		return nil, fmt.Errorf("configure agent: %w", err)
	}
	tools, err := resolveSandboxTools(cfg.sandboxTools)
	if err != nil {
		return nil, err
	}
	for _, t := range tools {
		log.Printf("pigo-server: sandbox tool %s: %s mounted read-only at %s", t.Name, t.HostDir, t.Mount)
	}
	bwrap, bwrapErr := findBwrap()
	sandbox := Sandbox{Bwrap: bwrap, Tools: tools}
	if bwrapErr != nil {
		log.Printf("pigo-server: sandbox unavailable: %v (bash tools will fail closed)", bwrapErr)
	}
	noTools, toolNames := configuredTools(cfg.tools)
	exts, err := loadToolsConfig(cfg.toolsConfig)
	if err != nil {
		return nil, err
	}
	for _, t := range exts.tools {
		how := "adds"
		if t.replaces {
			how = "replaces the built-in"
		}
		impl := "command: " + t.Command
		if t.Go != "" {
			impl = "go: " + t.Go
		}
		log.Printf("pigo-server: extension tool %s %s (%s)", t.Name, how, impl)
	}
	for _, r := range exts.rules {
		log.Printf("pigo-server: tool naming: provider %q model %q → profile %s", r.Provider, r.Model, r.Profile)
	}
	if toolsAreAll(cfg.tools) {
		// "all" takes the extensions in too.
		toolNames = append(toolNames, exts.addedNames()...)
	}
	sessions, err := loadSessions(db, cfg.dataDir)
	if err != nil {
		return nil, err
	}
	auth, err := newAuthStore(db)
	if err != nil {
		return nil, err
	}
	customModels, err := newCustomModelStore(db)
	if err != nil {
		return nil, err
	}
	credentials, err := newCredentialStore(db, os.Getenv(credentialSecretEnv))
	if err != nil {
		return nil, err
	}
	if !credentials.enabled() {
		log.Printf("pigo-server: %s is not set; stored provider API keys are disabled (the stored keys are left untouched)", credentialSecretEnv)
	}
	ledger, err := newLedgerStore(db)
	if err != nil {
		return nil, err
	}
	s := &apiServer{
		config:              cfg,
		sandbox:             sandbox,
		modelHTTPClient:     &http.Client{Timeout: 10 * time.Second},
		openRouterModelsURL: defaultOpenRouterModelsURL,
		providerName:        providerName,
		noTools:             noTools,
		toolNames:           toolNames,
		exts:                exts,
		reaperStop:          make(chan struct{}),
		auth:                auth,
		customModels:        customModels,
		credentials:         credentials,
		settings:            settings,
		ledger:              ledger,
		meter:               &meter{ledger: ledger, settings: settings},
		db:                  db,
		sessions:            sessions,
	}
	opened = true
	if cfg.idleTimeout > 0 || cfg.emptySessionTTL > 0 {
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
			// Names and mount points only: this endpoint is unauthenticated,
			// and host paths are none of a visitor's business.
			"tools": sandboxToolsResponse(s.sandbox.Tools),
		},
		// Names and kinds only, never commands or env.
		"extensions": s.exts.report(),
	})
}

func (s *apiServer) handleModels(w http.ResponseWriter, r *http.Request) {
	models, freeErr := s.usableModels(r, principalForRequest(r).UserID, "")
	if len(models) == 0 && freeErr != nil {
		writeError(w, http.StatusBadGateway, "load OpenRouter models: "+freeErr.Error())
		return
	}
	writeJSON(w, http.StatusOK, models)
}

func (s *apiServer) handleCommands(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionForRequest(r, r.PathValue("id")); !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, webCommands())
}

func (s *apiServer) handleCreateSession(w http.ResponseWriter, r *http.Request) {
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
	// The administrator's defaults win over the start-up flags: the flags are
	// only the seed the settings store falls back to, so reading config here
	// would make the 部署 panel's "默认模型" silently do nothing.
	defaults := s.settings.get()
	userID := principalForRequest(r).UserID
	model, thinking := defaults.DefaultModel, defaults.DefaultThinking
	providerName := s.providerName
	if _, resolved, err := s.resolveModel(userID, model, ""); err == nil {
		providerName = resolved
	}
	managed := &managedSession{
		paths: paths,
		meta: sessionMeta{
			ID:        id,
			UserID:    userID,
			Model:     model,
			Provider:  providerName,
			Thinking:  thinking,
			Tools:     append([]string(nil), s.toolNames...),
			CreatedAt: now,
			LastUsed:  now,
		},
	}
	if err := s.saveSession(managed.meta); err != nil {
		_ = s.removeSession(id, paths)
		writeError(w, http.StatusInternalServerError, "write session metadata")
		return
	}

	s.mu.Lock()
	if len(s.sessions) >= s.config.maxSessions {
		s.mu.Unlock()
		_ = s.removeSession(id, paths)
		writeError(w, http.StatusServiceUnavailable, "session limit reached")
		return
	}
	s.sessions[id] = managed
	s.mu.Unlock()

	writeJSON(w, http.StatusCreated, s.sessionJSON(managed))
}

func (s *apiServer) handleGetSession(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.sessionForRequest(r, r.PathValue("id"))
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
	_ = s.saveSession(managed.meta)
	writeJSON(w, http.StatusOK, s.sessionJSON(managed))
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
	if strings.TrimSpace(request.Model) == "" && strings.TrimSpace(request.Provider) != "" {
		writeError(w, http.StatusBadRequest, "provider requires model")
		return
	}

	managed, ok := s.sessionForRequest(r, r.PathValue("id"))
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
		if m, expired := s.expiredCustomModel(r, model); expired {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("custom model %q expired on %s; pick another model or renew it", m.ID, m.ExpiresAt))
			return
		}
		if _, err := applyModelForProvider(&managed.meta, model, request.Provider, s.resolverFor(principalForRequest(r).UserID)); err != nil {
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
	if err := s.saveSession(managed.meta); err != nil {
		writeError(w, http.StatusInternalServerError, "write session metadata")
		return
	}
	writeJSON(w, http.StatusOK, s.sessionJSON(managed))
}

func (s *apiServer) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	managed, ok := s.sessions[id]
	if ok {
		managed.mu.Lock()
		ok = !managed.closed && principalCanAccess(principalForRequest(r), managed.meta)
		managed.mu.Unlock()
	}
	if ok {
		delete(s.sessions, id)
	}
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	// A running turn is stopped and given a moment to finish before the files
	// go: it would otherwise write its transcript into a deleted directory.
	s.closeTurn(managed, errTurnSessionClosed)
	managed.mu.Lock()
	managed.closed = true
	managed.stopLive()
	paths := managed.paths
	managed.mu.Unlock()
	if err := s.removeSession(id, paths); err != nil {
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
	managed, ok := s.sessionForRequest(r, id)
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	// The session lock is held only to set the turn up: the turn itself runs
	// in the background, and this request merely subscribes to it.
	managed.mu.Lock()
	if managed.closed {
		managed.mu.Unlock()
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	managed.meta.LastUsed = time.Now().UTC()
	if m, expired := s.expiredCustomModel(r, managed.meta.Model); expired {
		managed.mu.Unlock()
		writeError(w, http.StatusBadRequest, fmt.Sprintf("custom model %q expired on %s; switch model before sending", m.ID, m.ExpiresAt))
		return
	}
	if managed.meta.Title == "" {
		managed.meta.Title = cleanSessionTitle(request.Prompt)
	}
	userID := principalForRequest(r).UserID
	prompt, prefix, complete, err := resolveWebInput(&managed.meta, request.Prompt, s.resolverFor(userID),
		func(filter string) string { return s.modelListing(r, userID, filter) })
	if err != nil {
		managed.mu.Unlock()
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = s.saveSession(managed.meta)
	stream := r.URL.Query().Get("stream") == "true"
	if complete {
		// A command answered locally (/model, /help …) needs no turn, and may
		// run while one is in progress: a setting change applies to the next.
		managed.mu.Unlock()
		if stream {
			writeStreamText(w, prefix)
		} else {
			writeJSON(w, http.StatusOK, map[string]string{"reply": prefix})
		}
		return
	}
	// Fail before starting a turn that could not run a single tool.
	if s.loopFn == nil && s.needsSandbox() {
		if err := s.sandbox.Ready(); err != nil {
			managed.mu.Unlock()
			writeError(w, http.StatusServiceUnavailable, "sandbox unavailable: "+err.Error())
			return
		}
	}
	run, snapshot, sub, err := s.startTurn(managed, prompt, prefix)
	managed.mu.Unlock()
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if stream {
		s.streamTurn(w, r, run, snapshot, sub, nil)
		return
	}
	s.waitTurn(w, r, run, sub)
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
		if spec, ok := s.exts.tool(name); ok && spec.Command != "" {
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
	managed, ok := s.sessionForRequest(r, r.PathValue("id"))
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
	managed, ok := s.sessionForRequest(r, r.PathValue("id"))
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
		s.closeTurn(managed, errTurnShutdown)
		managed.mu.Lock()
		managed.closed = true
		managed.stopLive()
		managed.mu.Unlock()
	}
	s.db.Close()
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

// sessionJSON is sessionResponse plus what only the server knows: the tool
// naming profile the session's current model gets.
func (s *apiServer) sessionJSON(managed *managedSession) map[string]any {
	out := sessionResponse(managed)
	if name := s.profileName(managed.meta.Provider, managed.meta.Model); name != "" {
		out["toolProfile"] = name
		out["toolNames"] = s.exts.profiles[name].wireNames()
	}
	return out
}

func sessionResponse(managed *managedSession) map[string]any {
	m := managed.meta
	tools := append([]string{}, m.Tools...)
	out := map[string]any{
		"id":            m.ID,
		"title":         m.Title,
		"model":         m.Model,
		"provider":      m.Provider,
		"thinking":      m.Thinking,
		"tools":         tools,
		"createdAt":     m.CreatedAt,
		"lastUsed":      m.LastUsed,
		"pigoSessionId": m.PigoSessionID,
		"sandbox":       "bwrap",
		"alive":         managed.liveAlive(),
	}
	// turn is the running (or just-finished) turn; lastTurn how the last one
	// ended, which survives restarts — including a turn a crash interrupted.
	if managed.turn != nil {
		out["turn"] = managed.turn.info()
	}
	if m.LastTurn != nil {
		out["lastTurn"] = m.LastTurn
	}
	return out
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

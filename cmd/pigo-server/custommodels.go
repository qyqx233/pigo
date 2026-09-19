package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/smallnest/pigo/internal/provider"
)

// Model scopes. A user entry belongs to one account and expires; a public entry
// is the deployment's own catalog, maintained by an administrator, visible to
// everyone and never expiring.
const (
	modelScopeUser   = "user"
	modelScopePublic = "public"
)

// customModel is an added model id. A user-scoped entry is private to its owner
// and carries a YYYY-MM-DD ExpiresAt: it stays valid through that day (UTC) and
// is rejected afterwards. A public entry has an empty UserID and ExpiresAt.
type customModel struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	Provider  string    `json:"provider"`
	UserID    string    `json:"userId"`
	ExpiresAt string    `json:"expiresAt"`
	CreatedAt time.Time `json:"createdAt"`
	// Scope is "user" or "public". Entries written before this field existed
	// decode with an empty value and are migrated to "user" on load, which is
	// what they were.
	Scope string `json:"scope,omitempty"`
}

func (m customModel) public() bool {
	return m.Scope == modelScopePublic
}

// expired reports whether a dated entry has passed its day. An entry with no
// expiry date (every public one) never expires — without this guard the string
// comparison below would call an empty date expired on sight.
func (m customModel) expired(now time.Time) bool {
	if strings.TrimSpace(m.ExpiresAt) == "" {
		return false
	}
	return now.UTC().Format("2006-01-02") > m.ExpiresAt
}

type customModelStore struct {
	mu     sync.Mutex
	path   string
	models []customModel
}

func newCustomModelStore(dataDir string) (*customModelStore, error) {
	dir := filepath.Join(dataDir, "models")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create models dir: %w", err)
	}
	store := &customModelStore{path: filepath.Join(dir, "models.json")}
	data, err := os.ReadFile(store.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return store, nil
		}
		return nil, fmt.Errorf("read custom models store: %w", err)
	}
	if err := json.Unmarshal(data, &store.models); err != nil {
		return nil, fmt.Errorf("decode custom models store: %w", err)
	}
	// Entries written before scopes existed were all user-scoped.
	migrated := false
	for i := range store.models {
		if store.models[i].Scope == "" {
			store.models[i].Scope = modelScopeUser
			migrated = true
		}
	}
	if migrated {
		if err := store.saveLocked(); err != nil {
			return nil, err
		}
	}
	return store, nil
}

// list returns one user's own entries.
func (s *customModelStore) list(userID string) []customModel {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]customModel, 0, len(s.models))
	for _, m := range s.models {
		if !m.public() && m.UserID == userID {
			out = append(out, m)
		}
	}
	return out
}

// listPublic returns the deployment's shared catalog.
func (s *customModelStore) listPublic() []customModel {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]customModel, 0, len(s.models))
	for _, m := range s.models {
		if m.public() {
			out = append(out, m)
		}
	}
	return out
}

// listVisible returns what one user may choose from: the shared catalog first,
// then their own entries. The order matters — callers deduplicate by id with
// first-wins, so a public entry takes precedence over a personal one with the
// same id, and a user cannot shadow the deployment's routing with an expired
// entry of their own.
func (s *customModelStore) listVisible(userID string) []customModel {
	out := s.listPublic()
	return append(out, s.list(userID)...)
}

// add upserts by (scope, userID, id): re-adding an existing id updates its
// label, provider, and expiry date, which doubles as the renewal path.
func (s *customModelStore) add(m customModel) error {
	// An entry with no scope is a user entry — the same rule the load-time
	// migration applies, enforced here too so it holds for every entry in the
	// store however it arrived.
	if m.Scope == "" {
		m.Scope = modelScopeUser
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.models {
		if existing.Scope == m.Scope && existing.UserID == m.UserID && existing.ID == m.ID {
			existing.Label = m.Label
			existing.Provider = m.Provider
			existing.ExpiresAt = m.ExpiresAt
			s.models[i] = existing
			return s.saveLocked()
		}
	}
	s.models = append(s.models, m)
	return s.saveLocked()
}

// remove deletes one user's entry.
func (s *customModelStore) remove(userID, id string) bool {
	return s.removeScoped(modelScopeUser, userID, id)
}

// removePublic deletes a shared-catalog entry.
func (s *customModelStore) removePublic(id string) bool {
	return s.removeScoped(modelScopePublic, "", id)
}

func (s *customModelStore) removeScoped(scope, userID, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, m := range s.models {
		if m.Scope == scope && m.UserID == userID && m.ID == id {
			s.models = append(s.models[:i], s.models[i+1:]...)
			_ = s.saveLocked()
			return true
		}
	}
	return false
}

// dropUser removes every entry belonging to a user, for account deletion. The
// shared catalog is untouched.
func (s *customModelStore) dropUser(userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.models[:0]
	removed := false
	for _, m := range s.models {
		if !m.public() && m.UserID == userID {
			removed = true
			continue
		}
		kept = append(kept, m)
	}
	s.models = kept
	if !removed {
		return nil
	}
	return s.saveLocked()
}

// find locates the entry that governs modelID for this user: their own first,
// then the shared catalog. A nil store answers "not found" so callers that run
// without a catalog (focused tests) need no guard of their own — the same
// contract credentialStore follows.
func (s *customModelStore) find(userID, modelID string) (customModel, bool) {
	if s == nil {
		return customModel{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.models {
		if !m.public() && m.UserID == userID && m.ID == modelID {
			return m, true
		}
	}
	for _, m := range s.models {
		if m.public() && m.ID == modelID {
			return m, true
		}
	}
	return customModel{}, false
}

func (s *customModelStore) saveLocked() error {
	data, err := json.MarshalIndent(s.models, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write custom models store: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit custom models store: %w", err)
	}
	return nil
}

// expiredCustomModel reports whether modelID names one of the caller's custom
// models whose expiry date has passed. Such models are disabled: switching to
// them or sending a message with them is rejected.
func (s *apiServer) expiredCustomModel(r *http.Request, modelID string) (customModel, bool) {
	if s.customModels == nil || strings.TrimSpace(modelID) == "" {
		return customModel{}, false
	}
	m, ok := s.customModels.find(principalForRequest(r).UserID, strings.TrimSpace(modelID))
	if ok && m.expired(time.Now().UTC()) {
		return m, true
	}
	return customModel{}, false
}

// visibleCustomModels maps the models the caller may choose onto the
// /api/models response shape: the deployment's shared catalog first, then the
// caller's own unexpired entries, tagged so the UI can tell them apart.
func (s *apiServer) visibleCustomModels(r *http.Request) []modelResponse {
	if s.customModels == nil {
		return nil
	}
	now := time.Now().UTC()
	out := make([]modelResponse, 0)
	seen := map[string]bool{}
	for _, m := range s.customModels.listVisible(principalForRequest(r).UserID) {
		if m.expired(now) || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		label := m.Label + " · 自定义"
		if m.public() {
			label = m.Label
		}
		out = append(out, modelResponse{ID: m.ID, Label: label, Provider: m.Provider})
	}
	return out
}

type customModelRequest struct {
	ID        string `json:"id"`
	Label     string `json:"label,omitempty"`
	Provider  string `json:"provider,omitempty"`
	ExpiresAt string `json:"expiresAt"`
}

type customModelResponse struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	Provider  string    `json:"provider"`
	ExpiresAt string    `json:"expiresAt"`
	Expired   bool      `json:"expired"`
	CreatedAt time.Time `json:"createdAt"`
	Scope     string    `json:"scope"`
}

func customModelToResponse(m customModel, now time.Time) customModelResponse {
	return customModelResponse{
		ID:        m.ID,
		Label:     m.Label,
		Provider:  m.Provider,
		ExpiresAt: m.ExpiresAt,
		Expired:   m.expired(now),
		CreatedAt: m.CreatedAt,
		Scope:     m.Scope,
	}
}

func (s *apiServer) handleListCustomModels(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	out := make([]customModelResponse, 0)
	for _, m := range s.customModels.list(principalForRequest(r).UserID) {
		out = append(out, customModelToResponse(m, now))
	}
	writeJSON(w, http.StatusOK, out)
}

// normalizeModelRequest validates and fills in a model submission. datedEntry
// distinguishes the two callers: a personal entry must carry an expiry date, a
// shared-catalog entry must not have one at all.
func normalizeModelRequest(request customModelRequest, datedEntry bool, resolve providerResolver) (customModelRequest, error) {
	request.ID = strings.TrimSpace(request.ID)
	if request.ID == "" {
		return request, errors.New("id is required")
	}
	request.ExpiresAt = strings.TrimSpace(request.ExpiresAt)
	if datedEntry {
		if _, err := time.Parse("2006-01-02", request.ExpiresAt); err != nil {
			return request, errors.New("expiresAt must be a YYYY-MM-DD date")
		}
	} else {
		// A shared entry is the deployment's own catalog and does not expire.
		request.ExpiresAt = ""
	}
	request.Provider = strings.TrimSpace(request.Provider)
	if _, _, err := resolve(request.ID, request.Provider); err != nil {
		return request, fmt.Errorf("cannot route model %q: %v", request.ID, err)
	}
	request.Label = strings.TrimSpace(request.Label)
	if request.Label == "" {
		request.Label = request.ID
	}
	if request.Provider == "" {
		request.Provider = "openrouter"
	}
	return request, nil
}

func (s *apiServer) handleAddCustomModel(w http.ResponseWriter, r *http.Request) {
	var request customModelRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	request, err := normalizeModelRequest(request, true, s.resolveProvider)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	m := customModel{
		ID:        request.ID,
		Label:     request.Label,
		Provider:  request.Provider,
		UserID:    principalForRequest(r).UserID,
		ExpiresAt: request.ExpiresAt,
		CreatedAt: time.Now().UTC(),
		Scope:     modelScopeUser,
	}
	if err := s.customModels.add(m); err != nil {
		writeError(w, http.StatusInternalServerError, "save custom model")
		return
	}
	writeJSON(w, http.StatusOK, customModelToResponse(m, time.Now().UTC()))
}

// --- the shared catalog (administrators) ------------------------------------

func (s *apiServer) handleListPublicModels(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().UTC()
	out := make([]customModelResponse, 0)
	for _, m := range s.customModels.listPublic() {
		out = append(out, customModelToResponse(m, now))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *apiServer) handleAddPublicModel(w http.ResponseWriter, r *http.Request) {
	var request customModelRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	request, err := normalizeModelRequest(request, false, s.resolveProvider)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	m := customModel{
		ID:        request.ID,
		Label:     request.Label,
		Provider:  request.Provider,
		CreatedAt: time.Now().UTC(),
		Scope:     modelScopePublic,
	}
	if err := s.customModels.add(m); err != nil {
		writeError(w, http.StatusInternalServerError, "save public model")
		return
	}
	s.logAdminAction(r, "add public model", m.ID)
	writeJSON(w, http.StatusOK, customModelToResponse(m, time.Now().UTC()))
}

func (s *apiServer) handleDeletePublicModel(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || !s.customModels.removePublic(id) {
		writeError(w, http.StatusNotFound, "public model not found")
		return
	}
	s.logAdminAction(r, "delete public model", id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *apiServer) handleDeleteCustomModel(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || !s.customModels.remove(principalForRequest(r).UserID, id) {
		writeError(w, http.StatusNotFound, "custom model not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// providerInfoResponse describes one built-in provider for the custom-model
// form: whether the server process currently holds its API key, and which env
// var to set when it does not.
type providerInfoResponse struct {
	Name    string `json:"name"`
	HasKey  bool   `json:"hasKey"`
	KeyHint string `json:"keyHint"`
	// Source names which tier supplies the key that would actually be used for
	// this request: "user", "public", "env" or "none". It lets the UI tell a
	// user whether they are spending their own quota or the shared pool's.
	Source string `json:"source"`
	// Custom marks an administrator-defined endpoint; Protocol and BaseURL are
	// then set. A built-in leaves all three at their zero values.
	Custom   bool   `json:"custom,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	BaseURL  string `json:"baseUrl,omitempty"`
}

// credentialSource names the tier that would supply providerName's key for
// userID: "user", "public", "env" or "none". It mirrors credentialStore.resolve
// so every surface — the provider table, /models — reports the tier a run will
// really use. envKey is whether the process environment carries a key; custom
// endpoints pass false, having no registry entry to derive one from.
func (s *apiServer) credentialSource(userID, providerName string, envKey bool) string {
	switch {
	case s.settings.get().AllowUserKeys && userID != "" && s.credentials.has(userID, providerName):
		return "user"
	case s.credentials.has("", providerName):
		return "public"
	case envKey:
		return "env"
	}
	return "none"
}

// envHasKey reports whether the process environment carries a key for a
// built-in provider. A provider that declares no variables (a local endpoint)
// needs none, so it counts as keyed.
func envHasKey(spec provider.ProviderSpec) bool {
	if len(spec.EnvVars) == 0 {
		return true
	}
	for _, env := range spec.EnvVars {
		if strings.TrimSpace(os.Getenv(env)) != "" {
			return true
		}
	}
	return false
}

func (s *apiServer) handleListProviders(w http.ResponseWriter, r *http.Request) {
	userID := principalForRequest(r).UserID
	specs := provider.ProviderSpecs()
	out := make([]providerInfoResponse, 0, len(specs))
	for _, spec := range specs {
		source := s.credentialSource(userID, spec.Name, envHasKey(spec))
		out = append(out, providerInfoResponse{
			Name:    spec.Name,
			HasKey:  source != "none",
			KeyHint: provider.APIKeyEnvHint(spec.Name),
			Source:  source,
		})
	}
	for _, custom := range s.settings.customProviders() {
		source := s.credentialSource(userID, custom.Name, false)
		out = append(out, providerInfoResponse{
			Name:     custom.Name,
			HasKey:   source != "none",
			KeyHint:  provider.APIKeyEnvHint(custom.Name),
			Source:   source,
			Custom:   true,
			Protocol: custom.Protocol,
			BaseURL:  custom.BaseURL,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// usableModels is the one list of models a user can run on this server, shared
// by the settings picker, the chat's model switcher and the /models command so
// they cannot disagree: models added here (shared catalog, then the user's own),
// OpenRouter's live free catalog, then the presets of every other provider that
// resolves to a key. The rest of provider.PresetCatalog — models on providers
// without a key, and OpenRouter's mostly paid static presets — is left out.
//
// A failure to reach OpenRouter is returned alongside whatever else is usable,
// so an outage there does not hide the providers that work.
func (s *apiServer) usableModels(r *http.Request, userID, filter string) ([]modelResponse, error) {
	filter = strings.ToLower(strings.TrimSpace(filter))
	var out []modelResponse
	seen := map[string]bool{}
	add := func(m modelResponse) {
		if !seen[m.ID] {
			seen[m.ID] = true
			out = append(out, m)
		}
	}

	for _, m := range s.visibleCustomModels(r) {
		if filter == "" || m.Provider == filter {
			m.Source = "custom"
			add(m)
		}
	}

	var freeErr error
	if filter == "" || filter == "openrouter" {
		free, err := s.fetchOpenRouterFreeModels(r)
		freeErr = err
		for _, m := range free {
			m.Source = "free"
			add(m)
		}
	}

	specs := map[string]provider.ProviderSpec{}
	for _, spec := range provider.ProviderSpecs() {
		specs[spec.Name] = spec
	}
	usable := map[string]bool{}
	for _, preset := range provider.PresetCatalog {
		if preset.Provider == "openrouter" || (filter != "" && preset.Provider != filter) {
			continue
		}
		ok, known := usable[preset.Provider]
		if !known {
			spec, builtin := specs[preset.Provider]
			ok = builtin && s.credentialSource(userID, preset.Provider, envHasKey(spec)) != "none"
			usable[preset.Provider] = ok
		}
		if ok {
			add(modelResponse{ID: preset.ID, Label: preset.Label(), Provider: preset.Provider, Source: "preset"})
		}
	}
	return out, freeErr
}

// modelListing renders usableModels for the /models command, one per line.
func (s *apiServer) modelListing(r *http.Request, userID, filter string) string {
	models, freeErr := s.usableModels(r, userID, filter)
	lines := make([]string, 0, len(models)+1)
	for _, m := range models {
		lines = append(lines, fmt.Sprintf("`%s` %s · %s", m.ID, m.Label, m.Provider))
	}
	if freeErr != nil {
		lines = append(lines, "（OpenRouter 免费目录暂时无法获取："+freeErr.Error()+"）")
	}
	if len(models) == 0 {
		return "没有可用的模型 —— 在 设置 → Provider 配置 Key，或在 设置 → 模型 添加"
	}
	return strings.Join(lines, "\n") + "\n\n用 /model <id> 切换"
}

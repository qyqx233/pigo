// Context windows and compaction thresholds: how big a model's context is, and
// at what share of it a session's history is summarized (compacted). There is a
// deployment-wide default for both, and an administrator can override either for
// any provider + model — the same shape as the price table, kept in the same
// settings document. OpenRouter's catalog supplies the window of its models
// when no override does.
//
// The threshold is a percentage of the window; the compaction package speaks in
// reserved tokens (it compacts past window − reserve), so the conversion
// happens here and internal/ is unchanged.
//
// See spec/context-compaction.md.
package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/smallnest/pigo/internal/compaction"
)

const (
	// defaultContextWindow and defaultCompactPct apply until an administrator
	// sets the deployment defaults: the CLI's window, and compaction at 80%.
	defaultContextWindow = 128000
	defaultCompactPct    = 80

	minContextWindow = 4000
	maxContextWindow = 10_000_000
	// The threshold's range: below 50% compaction comes early and often;
	// above 95% the reserve — which also bounds the summary's length — is too
	// small to write one.
	minCompactPct = 50
	maxCompactPct = 95

	// maxKeepRecentTokens caps the recent history kept verbatim; smaller
	// windows keep a quarter of themselves.
	maxKeepRecentTokens = 20000
)

// modelParam overrides the window and/or the threshold for one model. A zero
// field keeps the default.
type modelParam struct {
	Provider      string    `json:"provider"`
	Model         string    `json:"model"`
	ContextWindow int       `json:"contextWindow,omitempty"`
	CompactPct    int       `json:"compactPct,omitempty"`
	UpdatedBy     string    `json:"updatedBy,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt,omitempty"`
}

func validWindow(n int) bool { return n >= minContextWindow && n <= maxContextWindow }
func validPct(n int) bool    { return n >= minCompactPct && n <= maxCompactPct }

var (
	errWindowRange = fmt.Errorf("上下文窗口须在 %d 到 %d token 之间", minContextWindow, maxContextWindow)
	errPctRange    = fmt.Errorf("压缩阈值须在 %d%% 到 %d%% 之间", minCompactPct, maxCompactPct)
)

// validate normalizes a submitted override.
func (p modelParam) validate() (modelParam, error) {
	p.Provider = strings.ToLower(strings.TrimSpace(p.Provider))
	p.Model = strings.TrimSpace(p.Model)
	if p.Provider == "" || p.Model == "" {
		return p, errors.New("provider 和模型都不能为空")
	}
	if p.ContextWindow == 0 && p.CompactPct == 0 {
		return p, errors.New("上下文窗口和压缩阈值至少设一项")
	}
	if p.ContextWindow != 0 && !validWindow(p.ContextWindow) {
		return p, errWindowRange
	}
	if p.CompactPct != 0 && !validPct(p.CompactPct) {
		return p, errPctRange
	}
	return p, nil
}

// contextDefaults returns the deployment defaults, falling back to the
// built-in ones for fields never set.
func (s *settingsStore) contextDefaults() (window, pct int) {
	settings := s.get()
	window, pct = settings.DefaultContextWindow, settings.DefaultCompactPct
	if window == 0 {
		window = defaultContextWindow
	}
	if pct == 0 {
		pct = defaultCompactPct
	}
	return window, pct
}

// setContextDefaults stores the deployment defaults.
func (s *settingsStore) setContextDefaults(window, pct int) error {
	if !validWindow(window) {
		return errWindowRange
	}
	if !validPct(pct) {
		return errPctRange
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prevWindow, prevPct := s.settings.DefaultContextWindow, s.settings.DefaultCompactPct
	s.settings.DefaultContextWindow, s.settings.DefaultCompactPct = window, pct
	if err := s.saveLocked(); err != nil {
		s.settings.DefaultContextWindow, s.settings.DefaultCompactPct = prevWindow, prevPct
		return err
	}
	return nil
}

// modelParams returns the overrides.
func (s *settingsStore) modelParams() []modelParam {
	return s.get().ModelParams
}

// putModelParam adds or replaces an override.
func (s *settingsStore) putModelParam(next modelParam) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := append([]modelParam(nil), s.settings.ModelParams...)
	key := priceKey(next.Provider, next.Model)
	replaced := false
	for i, row := range s.settings.ModelParams {
		if priceKey(row.Provider, row.Model) == key {
			s.settings.ModelParams[i] = next
			replaced = true
			break
		}
	}
	if !replaced {
		s.settings.ModelParams = append(s.settings.ModelParams, next)
	}
	if err := s.saveLocked(); err != nil {
		s.settings.ModelParams = previous
		return err
	}
	return nil
}

// removeModelParam drops an override, reporting whether it existed.
func (s *settingsStore) removeModelParam(providerName, model string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := priceKey(providerName, model)
	kept := make([]modelParam, 0, len(s.settings.ModelParams))
	for _, row := range s.settings.ModelParams {
		if priceKey(row.Provider, row.Model) != key {
			kept = append(kept, row)
		}
	}
	if len(kept) == len(s.settings.ModelParams) {
		return false, nil
	}
	previous := s.settings.ModelParams
	s.settings.ModelParams = kept
	if err := s.saveLocked(); err != nil {
		s.settings.ModelParams = previous
		return false, err
	}
	return true, nil
}

// contextParams is what applies to one model, and where each value came from.
type contextParams struct {
	Window int `json:"window"`
	// CompactPct is the threshold, a percentage of Window.
	CompactPct int `json:"compactPct"`
	// WindowSource is "override", "openrouter" or "default"; PctSource is
	// "override" or "default".
	WindowSource string `json:"windowSource"`
	PctSource    string `json:"pctSource"`
}

// CompactAt is the context size at which a session compacts.
func (p contextParams) CompactAt() int { return p.Window * p.CompactPct / 100 }

// settings converts the parameters into the compaction package's terms. It
// compacts past window − reserve, so the reserve is the part above the
// threshold; the recent history kept verbatim is a quarter of the window, at
// most 20k tokens.
func (p contextParams) settings() compaction.CompactionSettings {
	keep := p.Window / 4
	if keep > maxKeepRecentTokens {
		keep = maxKeepRecentTokens
	}
	return compaction.CompactionSettings{
		Enabled:          true,
		ReserveTokens:    p.Window * (100 - p.CompactPct) / 100,
		KeepRecentTokens: keep,
	}
}

// contextParams resolves a model's window and threshold: an override, then —
// for OpenRouter's models — its catalog, then the deployment default.
func (s *apiServer) contextParams(providerName, model string) contextParams {
	window, pct := s.settings.contextDefaults()
	out := contextParams{Window: window, CompactPct: pct, WindowSource: "default", PctSource: "default"}
	key := priceKey(providerName, model)
	for _, row := range s.settings.modelParams() {
		if priceKey(row.Provider, row.Model) != key {
			continue
		}
		if row.ContextWindow > 0 {
			out.Window, out.WindowSource = row.ContextWindow, "override"
		}
		if row.CompactPct > 0 {
			out.CompactPct, out.PctSource = row.CompactPct, "override"
		}
	}
	if out.WindowSource == "default" && strings.EqualFold(strings.TrimSpace(providerName), "openrouter") {
		if n := s.openRouterWindows.lookup(s, model); n > 0 {
			out.Window, out.WindowSource = n, "openrouter"
		}
	}
	return out
}

// openRouterWindowTTL is how long OpenRouter's catalog windows are trusted.
const openRouterWindowTTL = time.Hour

// openRouterWindowCache keeps the context length of OpenRouter's models, from
// its catalog. It is filled whenever the catalog is fetched for the model list;
// a turn that finds it empty or stale uses the default window and refreshes it
// in the background rather than wait for the network.
type openRouterWindowCache struct {
	mu         sync.Mutex
	windows    map[string]int
	fetched    time.Time
	refreshing bool
}

// store records a catalog's windows.
func (c *openRouterWindowCache) store(windows map[string]int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.windows, c.fetched = windows, time.Now()
}

// lookup returns a model's window, 0 when unknown, starting a refresh when the
// cache is empty or stale.
func (c *openRouterWindowCache) lookup(s *apiServer, model string) int {
	c.mu.Lock()
	window := c.windows[model]
	stale := c.windows == nil || time.Since(c.fetched) > openRouterWindowTTL
	start := stale && !c.refreshing && s.modelHTTPClient != nil
	if start {
		c.refreshing = true
	}
	c.mu.Unlock()
	if start {
		go func() {
			defer func() {
				c.mu.Lock()
				c.refreshing = false
				c.mu.Unlock()
			}()
			request, err := http.NewRequest(http.MethodGet, "/", nil)
			if err != nil {
				return
			}
			_, _ = s.fetchOpenRouterFreeModels(request) // fills the cache
		}()
	}
	return window
}

// --- API -------------------------------------------------------------------------

// modelParamsResponse is the whole configuration, for the settings page.
type modelParamsResponse struct {
	DefaultWindow int          `json:"defaultWindow"`
	DefaultPct    int          `json:"defaultCompactPct"`
	Params        []modelParam `json:"params"`
}

func (s *apiServer) modelParamsList() modelParamsResponse {
	window, pct := s.settings.contextDefaults()
	params := s.settings.modelParams()
	if params == nil {
		params = []modelParam{}
	}
	return modelParamsResponse{DefaultWindow: window, DefaultPct: pct, Params: params}
}

func (s *apiServer) handleListModelParams(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.modelParamsList())
}

func (s *apiServer) handlePutContextDefaults(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Window int `json:"defaultWindow"`
		Pct    int `json:"defaultCompactPct"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.settings.setContextDefaults(request.Window, request.Pct); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.logAdminAction(r, "set context defaults", fmt.Sprintf("window=%d compact=%d%%", request.Window, request.Pct))
	writeJSON(w, http.StatusOK, s.modelParamsList())
}

func (s *apiServer) handlePutModelParam(w http.ResponseWriter, r *http.Request) {
	var request modelParam
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	next, err := request.validate()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	next.UpdatedBy = principalForRequest(r).Username
	next.UpdatedAt = time.Now().UTC()
	if err := s.settings.putModelParam(next); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logAdminAction(r, "put model params", fmt.Sprintf("%s/%s window=%d compact=%d%%", next.Provider, next.Model, next.ContextWindow, next.CompactPct))
	writeJSON(w, http.StatusOK, s.modelParamsList())
}

// handleDeleteModelParam takes the row in the query string, like the price
// table: model ids contain slashes.
func (s *apiServer) handleDeleteModelParam(w http.ResponseWriter, r *http.Request) {
	providerName := strings.TrimSpace(r.URL.Query().Get("provider"))
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if providerName == "" || model == "" {
		writeError(w, http.StatusBadRequest, "provider 和 model 都不能为空")
		return
	}
	removed, err := s.settings.removeModelParam(providerName, model)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, "该模型没有单独设置")
		return
	}
	s.logAdminAction(r, "delete model params", providerName+"/"+model)
	writeJSON(w, http.StatusOK, s.modelParamsList())
}

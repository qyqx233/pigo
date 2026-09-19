// Deployment-wide settings an administrator maintains at runtime, replacing
// values that were previously fixed at process start.
//
// The document holds no secrets — provider keys live in credentials.json,
// encrypted (see credentials.go) — so this file is plain JSON and readable by
// anyone who can read the data directory, same as the session transcripts
// already there.
//
// Defaults come from the command line, so a deployment that never opens the
// admin console behaves exactly as it did before. The file is only written when
// an administrator changes something; until then there is nothing on disk to
// migrate or conflict with.
package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/smallnest/pigo/internal/provider"
)

// serverSettings is the stored document. The booleans carry no omitempty: this
// file is only ever written by this struct, so every field is always present
// and a decoded `false` always means a real "off" rather than an absent key.
type serverSettings struct {
	// DefaultModel and DefaultThinking seed new sessions.
	DefaultModel    string `json:"defaultModel"`
	DefaultThinking string `json:"defaultThinking"`
	// AllowRegistration gates self-service signup. A request carrying the
	// service token bypasses it, so an administrator can still add accounts to a
	// closed deployment.
	AllowRegistration bool `json:"allowRegistration"`
	// AllowUserKeys gates personal provider keys. Turning it off stops stored
	// user keys from being used; it does not delete them.
	AllowUserKeys bool `json:"allowUserKeys"`
	// CustomProviders are endpoints this deployment reaches that are not in the
	// built-in registry: a self-hosted gateway, a proxy, anything speaking the
	// OpenAI or Anthropic wire format. They are administrator-managed because
	// pointing the server at an arbitrary URL is a deployment decision, not a
	// per-user preference.
	CustomProviders []customProvider `json:"customProviders,omitempty"`
	// ModelPrices is the price table billing applies (billing_price.go). Like
	// the custom providers it is maintained through its own endpoints, not the
	// settings form.
	ModelPrices []modelPrice `json:"modelPrices,omitempty"`
	// DefaultContextWindow and DefaultCompactPct are the deployment's context
	// window and compaction threshold (a percentage of the window); zero means
	// the built-in 128k and 80%. ModelParams overrides them per model. Like the
	// price table they have their own endpoints (context_params.go).
	DefaultContextWindow int          `json:"defaultContextWindow,omitempty"`
	DefaultCompactPct    int          `json:"defaultCompactPct,omitempty"`
	ModelParams          []modelParam `json:"modelParams,omitempty"`
}

// customProvider is one non-registry endpoint. The API key is not here: it
// lives in the encrypted credential store under Name, exactly like a built-in
// provider's key.
type customProvider struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	BaseURL  string `json:"baseUrl"`
	// ConversationID sends the session id as conversation_id in every request
	// body (conversation_id.go). CodeBuddy gateways such as workbuddy2api use
	// it to keep a conversation on one upstream cache; without it their prompt
	// cache almost never hits. OpenAI-protocol endpoints only.
	ConversationID bool `json:"conversationId,omitempty"`
}

// customProviderNamePattern keeps a custom name usable as a credential-store
// key and an environment-variable stem, and distinguishable from a built-in.
var customProviderNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,31}$`)

// validate normalizes a submitted custom provider.
func (c customProvider) validate() (customProvider, error) {
	c.Name = strings.ToLower(strings.TrimSpace(c.Name))
	c.BaseURL = strings.TrimSpace(c.BaseURL)
	c.Protocol = strings.ToLower(strings.TrimSpace(c.Protocol))
	if !customProviderNamePattern.MatchString(c.Name) {
		return c, errors.New("名称须为 2-32 位小写字母、数字、下划线或短横线")
	}
	if func() bool { _, ok := provider.LookupProviderSpec(c.Name); return ok }() {
		return c, errors.New("该名称与内置 provider 冲突：" + c.Name)
	}
	if c.Protocol != provider.ProtocolOpenAI && c.Protocol != provider.ProtocolAnthropic {
		return c, errors.New("协议须为 openai 或 anthropic")
	}
	if c.ConversationID && c.Protocol != provider.ProtocolOpenAI {
		// Only the OpenAI request encoder carries extra body fields.
		return c, errors.New("会话 ID 只支持 openai 协议的端点")
	}
	parsed, err := url.Parse(c.BaseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return c, errors.New("base_url 须为 http(s):// 开头的完整地址")
	}
	return c, nil
}

// settingsStore keeps the deployment settings as one JSON document, the
// "server" row of the settings table. It is small and changed by hand, so it
// is read and written whole.
type settingsStore struct {
	mu       sync.RWMutex
	db       *sqlDB
	settings serverSettings
}

const serverSettingsName = "server"

// newSettingsStore loads the document, falling back to the defaults derived
// from cfg when the file does not exist.
func newSettingsStore(db *sqlDB, cfg serverConfig) (*settingsStore, error) {
	store := &settingsStore{
		db: db,
		settings: serverSettings{
			DefaultModel:      cfg.model,
			DefaultThinking:   cfg.thinking,
			AllowRegistration: true,
			AllowUserKeys:     true,
		},
	}
	var doc string
	found := false
	if err := db.query("SELECT value FROM settings WHERE name = ?", func(rows *sql.Rows) error {
		found = true
		return rows.Scan(&doc)
	}, serverSettingsName); err != nil {
		return nil, fmt.Errorf("read settings: %w", err)
	}
	if !found {
		return store, nil
	}
	if err := json.Unmarshal([]byte(doc), &store.settings); err != nil {
		return nil, fmt.Errorf("decode settings: %w", err)
	}
	if strings.TrimSpace(store.settings.DefaultModel) == "" {
		store.settings.DefaultModel = cfg.model
	}
	if strings.TrimSpace(store.settings.DefaultThinking) == "" {
		store.settings.DefaultThinking = cfg.thinking
	}
	return store, nil
}

// get returns a copy of the current settings.
func (s *settingsStore) get() serverSettings {
	if s == nil {
		return serverSettings{AllowRegistration: true, AllowUserKeys: true}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.settings
}

// update applies next and persists it. Blank model/thinking values keep the
// current ones rather than erasing them, so a partial form submission cannot
// leave the deployment without a default model.
func (s *settingsStore) update(next serverSettings) (serverSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	merged := s.settings
	if v := strings.TrimSpace(next.DefaultModel); v != "" {
		merged.DefaultModel = v
	}
	if v := strings.TrimSpace(next.DefaultThinking); v != "" {
		merged.DefaultThinking = v
	}
	merged.AllowRegistration = next.AllowRegistration
	merged.AllowUserKeys = next.AllowUserKeys
	// Custom providers and the price table have their own endpoints and are not
	// part of this form; merged already carries them over unchanged.

	previous := s.settings
	s.settings = merged
	if err := s.saveLocked(); err != nil {
		s.settings = previous
		return previous, err
	}
	return merged, nil
}

func (s *settingsStore) saveLocked() error {
	return saveSettingsDoc(s.db, s.settings)
}

func saveSettingsDoc(e sqlExec, settings serverSettings) error {
	data, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	if _, err := e.exec(`INSERT INTO settings (name, value) VALUES (?, ?)
ON CONFLICT (name) DO UPDATE SET value = excluded.value`, serverSettingsName, string(data)); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	return nil
}

// customProviders returns the configured non-registry endpoints.
func (s *settingsStore) customProviders() []customProvider {
	return s.get().CustomProviders
}

// findCustomProvider looks one up by name.
func (s *settingsStore) findCustomProvider(name string) (customProvider, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, item := range s.customProviders() {
		if item.Name == name {
			return item, true
		}
	}
	return customProvider{}, false
}

// putCustomProvider adds or replaces one by name.
func (s *settingsStore) putCustomProvider(next customProvider) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := append([]customProvider(nil), s.settings.CustomProviders...)
	replaced := false
	for i, item := range s.settings.CustomProviders {
		if item.Name == next.Name {
			s.settings.CustomProviders[i] = next
			replaced = true
			break
		}
	}
	if !replaced {
		s.settings.CustomProviders = append(s.settings.CustomProviders, next)
	}
	if err := s.saveLocked(); err != nil {
		s.settings.CustomProviders = previous
		return err
	}
	return nil
}

// removeCustomProvider drops one, reporting whether it existed.
func (s *settingsStore) removeCustomProvider(name string) (bool, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := make([]customProvider, 0, len(s.settings.CustomProviders))
	found := false
	for _, item := range s.settings.CustomProviders {
		if item.Name == name {
			found = true
			continue
		}
		kept = append(kept, item)
	}
	if !found {
		return false, nil
	}
	previous := s.settings.CustomProviders
	s.settings.CustomProviders = kept
	if err := s.saveLocked(); err != nil {
		s.settings.CustomProviders = previous
		return false, err
	}
	return true, nil
}

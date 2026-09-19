package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func patchProvider(t *testing.T, server *apiServer, name string, next customProvider) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(next)
	request := adminRequest(t, http.MethodPatch, "/api/admin/providers/"+name, string(body))
	request.SetPathValue("name", name)
	response := httptest.NewRecorder()
	server.handlePatchCustomProvider(response, request)
	return response
}

// TestRenameCustomProvider verifies a rename carries along everything filed
// under the name — keys, models, prices, sessions — and survives a reload.
func TestRenameCustomProvider(t *testing.T) {
	server := newTestServer(t)
	if err := server.settings.putCustomProvider(customProvider{Name: "bonai", Protocol: "openai", BaseURL: "http://10.0.0.5:8000/v1"}); err != nil {
		t.Fatal(err)
	}
	if err := server.credentials.setPublicKey("bonai", "sk-public"); err != nil {
		t.Fatal(err)
	}
	if err := server.credentials.setUserKey("u1", "bonai", "sk-mine"); err != nil {
		t.Fatal(err)
	}
	if err := server.customModels.add(customModel{ID: "bonsai2", Label: "Bonsai", Provider: "bonai", Scope: modelScopePublic}); err != nil {
		t.Fatal(err)
	}
	if err := server.settings.putPrice(modelPrice{Provider: "bonai", Model: "bonsai2", Input: 1, Output: 2}); err != nil {
		t.Fatal(err)
	}
	settings := server.settings.get()
	settings.DefaultModel = "bonsai2"
	if _, err := server.settings.update(settings); err != nil {
		t.Fatal(err)
	}
	id := createSession(t, server)
	server.mu.RLock()
	managed := server.sessions[id]
	server.mu.RUnlock()
	if managed.meta.Provider != "bonai" {
		t.Fatalf("session provider = %q", managed.meta.Provider)
	}

	response := patchProvider(t, server, "bonai", customProvider{Name: "bonsai-gw", Protocol: "anthropic", BaseURL: "http://10.0.0.6:9000"})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}

	check := func(label string, s *apiServer, sessionProvider string) {
		t.Helper()
		if _, ok := s.settings.findCustomProvider("bonai"); ok {
			t.Errorf("%s: the old name survived", label)
		}
		got, ok := s.settings.findCustomProvider("bonsai-gw")
		if !ok || got.Protocol != "anthropic" || got.BaseURL != "http://10.0.0.6:9000" {
			t.Errorf("%s: renamed provider = %+v, %v", label, got, ok)
		}
		if key := s.credentials.resolve("", "bonsai-gw", true); key != "sk-public" {
			t.Errorf("%s: public key = %q", label, key)
		}
		if key := s.credentials.resolve("u1", "bonsai-gw", true); key != "sk-mine" {
			t.Errorf("%s: user key = %q", label, key)
		}
		if s.credentials.has("", "bonai") || s.credentials.has("u1", "bonai") {
			t.Errorf("%s: keys left under the old name", label)
		}
		if m, ok := s.customModels.find("", "bonsai2"); !ok || m.Provider != "bonsai-gw" {
			t.Errorf("%s: model = %+v", label, m)
		}
		if _, ok := s.settings.priceFor("bonsai-gw", "", "bonsai2"); !ok {
			t.Errorf("%s: price not moved", label)
		}
		if sessionProvider != "bonsai-gw" {
			t.Errorf("%s: session provider = %q", label, sessionProvider)
		}
	}
	managed.mu.Lock()
	provider := managed.meta.Provider
	managed.mu.Unlock()
	check("memory", server, provider)

	// What is stored agrees with what is in memory.
	reloaded := &apiServer{db: server.db}
	var err error
	if reloaded.settings, err = newSettingsStore(server.db, serverConfig{model: "x", thinking: "medium"}); err != nil {
		t.Fatal(err)
	}
	if reloaded.credentials, err = newCredentialStore(server.db, "test-master-secret"); err != nil {
		t.Fatal(err)
	}
	if reloaded.customModels, err = newCustomModelStore(server.db); err != nil {
		t.Fatal(err)
	}
	sessions, err := loadSessions(server.db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if sessions[id] == nil {
		t.Fatal("session not stored")
	}
	check("reloaded", reloaded, sessions[id].meta.Provider)
}

// TestEditCustomProviderRefuses covers the conflicts: an unknown provider, a
// name already taken, and a session mid-turn on the provider.
func TestEditCustomProviderRefuses(t *testing.T) {
	server := newTestServer(t)
	for _, name := range []string{"gw-a", "gw-b"} {
		if err := server.settings.putCustomProvider(customProvider{Name: name, Protocol: "openai", BaseURL: "http://10.0.0.5:8000/v1"}); err != nil {
			t.Fatal(err)
		}
	}
	if response := patchProvider(t, server, "nope", customProvider{Name: "nope2", Protocol: "openai", BaseURL: "http://x/v1"}); response.Code != http.StatusNotFound {
		t.Errorf("unknown: status = %d", response.Code)
	}
	if response := patchProvider(t, server, "gw-a", customProvider{Name: "gw-b", Protocol: "openai", BaseURL: "http://x/v1"}); response.Code != http.StatusConflict {
		t.Errorf("taken: status = %d", response.Code)
	}
	if response := patchProvider(t, server, "gw-a", customProvider{Name: "openai", Protocol: "openai", BaseURL: "http://x/v1"}); response.Code != http.StatusBadRequest {
		t.Errorf("built-in name: status = %d", response.Code)
	}

	id := createSession(t, server)
	server.mu.RLock()
	managed := server.sessions[id]
	server.mu.RUnlock()
	managed.mu.Lock()
	managed.meta.Provider = "gw-a"
	managed.turn = newTurnRun(id, turnLimits{max: time.Minute})
	managed.mu.Unlock()
	if response := patchProvider(t, server, "gw-a", customProvider{Name: "gw-c", Protocol: "openai", BaseURL: "http://x/v1"}); response.Code != http.StatusConflict {
		t.Errorf("mid-turn: status = %d %s", response.Code, response.Body.String())
	}
	if _, ok := server.settings.findCustomProvider("gw-a"); !ok {
		t.Error("a refused rename changed the provider")
	}

	// Editing only the address keeps the name.
	managed.mu.Lock()
	managed.turn = nil
	managed.mu.Unlock()
	if response := patchProvider(t, server, "gw-a", customProvider{Name: "gw-a", Protocol: "anthropic", BaseURL: "http://10.0.0.9/v1"}); response.Code != http.StatusOK {
		t.Fatalf("edit: status = %d %s", response.Code, response.Body.String())
	}
	if got, _ := server.settings.findCustomProvider("gw-a"); got.BaseURL != "http://10.0.0.9/v1" || got.Protocol != "anthropic" {
		t.Errorf("edited = %+v", got)
	}
}

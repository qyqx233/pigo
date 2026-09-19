package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCustomModelStoreAddListRemoveAndReload(t *testing.T) {
	forEachDB(t, testCustomModelStore)
}

func testCustomModelStore(t *testing.T, target dbTarget) {
	db := mustOpen(t, target)
	store, err := newCustomModelStore(db)
	if err != nil {
		t.Fatal(err)
	}
	entry := customModel{
		ID:        "stealth/union-alpha",
		Label:     "Union Alpha",
		Provider:  "openrouter",
		UserID:    "u1",
		ExpiresAt: time.Now().UTC().AddDate(0, 0, 7).Format("2006-01-02"),
		CreatedAt: time.Now().UTC(),
	}
	if err := store.add(entry); err != nil {
		t.Fatal(err)
	}
	if err := store.add(customModel{ID: "other/model", UserID: "u2", ExpiresAt: entry.ExpiresAt}); err != nil {
		t.Fatal(err)
	}
	owns := store.list("u1")
	if len(owns) != 1 || owns[0].ID != "stealth/union-alpha" {
		t.Fatalf("list(u1) = %+v", owns)
	}

	// Re-adding the same id renews the expiry instead of duplicating.
	renewed := entry
	renewed.ExpiresAt = time.Now().UTC().AddDate(0, 1, 0).Format("2006-01-02")
	if err := store.add(renewed); err != nil {
		t.Fatal(err)
	}
	owns = store.list("u1")
	if len(owns) != 1 || owns[0].ExpiresAt != renewed.ExpiresAt {
		t.Fatalf("after renew list(u1) = %+v", owns)
	}

	db.Close()
	db = mustOpen(t, target)
	reloaded, err := newCustomModelStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.list("u1"); len(got) != 1 || got[0].ExpiresAt != renewed.ExpiresAt {
		t.Fatalf("reloaded list(u1) = %+v", got)
	}

	if !reloaded.remove("u1", "stealth/union-alpha") {
		t.Fatal("remove returned false")
	}
	if reloaded.remove("u1", "stealth/union-alpha") {
		t.Fatal("second remove returned true")
	}
	if got := reloaded.list("u2"); len(got) != 1 {
		t.Fatalf("remove leaked across users: %+v", got)
	}
}

func TestCustomModelExpiryBoundary(t *testing.T) {
	now := time.Date(2026, 9, 17, 23, 59, 0, 0, time.UTC)
	if (customModel{ExpiresAt: "2026-09-17"}).expired(now) {
		t.Fatal("model expiring today must still be valid")
	}
	if !(customModel{ExpiresAt: "2026-09-16"}).expired(now) {
		t.Fatal("model expiring yesterday must be expired")
	}
}

func addCustomModelForTest(t *testing.T, server *apiServer, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/custom-models", strings.NewReader(body))
	response := httptest.NewRecorder()
	server.handleAddCustomModel(response, request)
	return response
}

func TestHandleAddCustomModelValidatesInput(t *testing.T) {
	server := newTestServer(t)
	var err error
	server.customModels, err = newCustomModelStore(server.db)
	if err != nil {
		t.Fatal(err)
	}

	if resp := addCustomModelForTest(t, server, `{"id":"","expiresAt":"2027-01-01"}`); resp.Code != http.StatusBadRequest {
		t.Fatalf("empty id: %d %s", resp.Code, resp.Body.String())
	}
	if resp := addCustomModelForTest(t, server, `{"id":"stealth/union-alpha","expiresAt":"next week"}`); resp.Code != http.StatusBadRequest {
		t.Fatalf("bad date: %d %s", resp.Code, resp.Body.String())
	}
	if resp := addCustomModelForTest(t, server, `{"id":"x/y","provider":"no-such-provider","expiresAt":"2027-01-01"}`); resp.Code != http.StatusBadRequest {
		t.Fatalf("unroutable id: %d %s", resp.Code, resp.Body.String())
	}

	resp := addCustomModelForTest(t, server, `{"id":"stealth/union-alpha","expiresAt":"2027-01-01"}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("add: %d %s", resp.Code, resp.Body.String())
	}
	var added customModelResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &added); err != nil {
		t.Fatal(err)
	}
	if added.ID != "stealth/union-alpha" || added.Label != "stealth/union-alpha" || added.Provider != "openrouter" || added.Expired {
		t.Fatalf("added = %+v", added)
	}
}

func TestHandleModelsMergesUnexpiredCustomModels(t *testing.T) {
	clearProviderKeys(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[
			{"id":"free/model:free","name":"Free Model","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]}}
		]}`)
	}))
	defer upstream.Close()

	server := &apiServer{modelHTTPClient: upstream.Client(), openRouterModelsURL: upstream.URL}
	var err error
	server.customModels, err = newCustomModelStore(openTestDB(t))
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC()
	_ = server.customModels.add(customModel{ID: "stealth/union-alpha", Label: "Union Alpha", Provider: "openrouter", ExpiresAt: today.Format("2006-01-02")})
	_ = server.customModels.add(customModel{ID: "old/model", Label: "Old", Provider: "openrouter", ExpiresAt: today.AddDate(0, 0, -1).Format("2006-01-02")})
	_ = server.customModels.add(customModel{ID: "free/model:free", Label: "Duplicate", Provider: "openrouter", ExpiresAt: today.Format("2006-01-02")})
	_ = server.customModels.add(customModel{ID: "other-user/model", Label: "Not Mine", Provider: "openrouter", UserID: "u2", ExpiresAt: today.Format("2006-01-02")})

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
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
		if m.ID == "stealth/union-alpha" && !strings.Contains(m.Label, "自定义") {
			t.Fatalf("custom model not tagged: %+v", m)
		}
	}
	// Models added on this server come first, then OpenRouter's free catalog.
	want := []string{"stealth/union-alpha", "free/model:free"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}

func TestHandleModelsServesCustomsWhenOpenRouterFails(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	server := &apiServer{modelHTTPClient: upstream.Client(), openRouterModelsURL: upstream.URL}
	var err error
	server.customModels, err = newCustomModelStore(openTestDB(t))
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC().Format("2006-01-02")
	_ = server.customModels.add(customModel{ID: "stealth/union-alpha", Label: "Union Alpha", Provider: "openrouter", ExpiresAt: today})

	request := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	response := httptest.NewRecorder()
	server.handleModels(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "stealth/union-alpha") {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
}

func TestUpdateSessionRejectsExpiredCustomModel(t *testing.T) {
	server := newTestServer(t)
	var err error
	server.customModels, err = newCustomModelStore(server.db)
	if err != nil {
		t.Fatal(err)
	}
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	_ = server.customModels.add(customModel{ID: "stealth/union-alpha", Label: "Union Alpha", Provider: "openrouter", ExpiresAt: yesterday})

	id := createSession(t, server)
	body := strings.NewReader(`{"model":"stealth/union-alpha","thinking":"medium"}`)
	request := httptest.NewRequest(http.MethodPatch, "/api/sessions/"+id, body)
	request.SetPathValue("id", id)
	response := httptest.NewRecorder()
	server.handleUpdateSession(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "expired") {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
}

func TestHandleMessageRejectsExpiredCustomModel(t *testing.T) {
	server := newTestServer(t)
	var err error
	server.customModels, err = newCustomModelStore(server.db)
	if err != nil {
		t.Fatal(err)
	}
	id := createSession(t, server)
	managed, _ := server.getSession(id)
	managed.meta.Model = "stealth/union-alpha"
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	_ = server.customModels.add(customModel{ID: "stealth/union-alpha", Label: "Union Alpha", Provider: "openrouter", ExpiresAt: yesterday})

	body := strings.NewReader(`{"prompt":"hello"}`)
	request := httptest.NewRequest(http.MethodPost, "/api/sessions/"+id+"/messages", body)
	request.SetPathValue("id", id)
	response := httptest.NewRecorder()
	server.handleMessage(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "expired") {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
}

// TestHandleListProviders pins the key rule: a provider counts as configured
// only by a stored key. A key in the server's environment does not enable it.
func TestHandleListProviders(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "env-key")
	t.Setenv("DEEPSEEK_API_KEY", "env-key")
	server := newTestServer(t)
	if err := server.credentials.setPublicKey("deepseek", "sk-stored"); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/providers", nil)
	response := httptest.NewRecorder()
	server.handleListProviders(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
	var providers []providerInfoResponse
	if err := json.Unmarshal(response.Body.Bytes(), &providers); err != nil {
		t.Fatal(err)
	}
	sources := map[string]providerInfoResponse{}
	for _, p := range providers {
		sources[p.Name] = p
	}
	if p := sources["openrouter"]; p.HasKey || p.Source != "none" {
		t.Errorf("openrouter with only an env key = %+v, want none", p)
	}
	if p := sources["deepseek"]; !p.HasKey || p.Source != "public" {
		t.Errorf("deepseek with a stored key = %+v, want public", p)
	}
	if strings.Contains(response.Body.String(), "_API_KEY") {
		t.Error("the response still names environment variables")
	}
}

func TestHandleDeleteCustomModel(t *testing.T) {
	server := newTestServer(t)
	var err error
	server.customModels, err = newCustomModelStore(server.db)
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC().Format("2006-01-02")
	_ = server.customModels.add(customModel{ID: "stealth/union-alpha", Label: "Union Alpha", Provider: "openrouter", ExpiresAt: today})

	request := httptest.NewRequest(http.MethodDelete, "/api/custom-models/stealth/union-alpha", nil)
	request.SetPathValue("id", "stealth/union-alpha")
	response := httptest.NewRecorder()
	server.handleDeleteCustomModel(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
	if got := server.customModels.list(""); len(got) != 0 {
		t.Fatalf("list after delete = %+v", got)
	}

	request = httptest.NewRequest(http.MethodDelete, "/api/custom-models/stealth/union-alpha", nil)
	request.SetPathValue("id", "stealth/union-alpha")
	response = httptest.NewRecorder()
	server.handleDeleteCustomModel(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("second delete: %d", response.Code)
	}
}

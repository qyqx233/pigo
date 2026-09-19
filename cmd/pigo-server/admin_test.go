package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseAdminRoster(t *testing.T) {
	roster := parseAdminRoster(" Alice, ,bob ,, CAROL ")
	for _, name := range []string{"alice", "Alice", "ALICE", "bob", "carol"} {
		if !roster.has(name) {
			t.Errorf("%q should be an admin", name)
		}
	}
	if roster.has("dave") {
		t.Error("dave should not be an admin")
	}
	if got := roster.describe(); got != "alice, bob, carol" {
		t.Errorf("describe = %q", got)
	}

	// An empty roster means no administrators at all, and says so.
	empty := parseAdminRoster("")
	if empty.has("alice") {
		t.Error("an empty roster must grant nobody")
	}
	if !strings.Contains(empty.describe(), "PIGO_ADMIN_USERS") {
		t.Errorf("describe = %q, want it to name the variable", empty.describe())
	}
	if parseAdminRoster("  ,, ").has("") {
		t.Error("blank entries must not grant the empty username")
	}
}

// TestRequireAdmin covers the three principals that reach the middleware.
func TestRequireAdmin(t *testing.T) {
	server := newTestServer(t)
	guarded := server.requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	cases := []struct {
		name      string
		principal requestPrincipal
		want      int
	}{
		{"admin", requestPrincipal{UserID: "u1", Username: "alice", Admin: true}, http.StatusTeapot},
		{"service token", requestPrincipal{Service: true}, http.StatusTeapot},
		{"plain user", requestPrincipal{UserID: "u2", Username: "bob"}, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
			request = request.WithContext(withPrincipal(request, tc.principal))
			response := httptest.NewRecorder()
			guarded.ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Errorf("status = %d, want %d (%s)", response.Code, tc.want, response.Body.String())
			}
		})
	}
}

// TestAdminRoleComesFromTheEnvironment verifies the role is resolved per
// request from the roster, not from anything stored in auth.json.
func TestAdminRoleComesFromTheEnvironment(t *testing.T) {
	server := newTestServer(t)
	server.config.admins = parseAdminRoster("alice")
	if _, _, _, err := server.auth.register("alice", "password-1234"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := server.auth.register("bob", "password-1234"); err != nil {
		t.Fatal(err)
	}

	// The stored record carries no role.
	raw, err := json.Marshal(server.auth.state.Users[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(raw)), "admin") {
		t.Errorf("the role must not be persisted: %s", raw)
	}

	// /api/auth/me reflects the roster.
	for _, tc := range []struct {
		username string
		want     bool
	}{{"alice", true}, {"bob", false}} {
		request := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
		request = request.WithContext(withPrincipal(request, requestPrincipal{
			UserID:   "u",
			Username: tc.username,
			Admin:    server.config.admins.has(tc.username),
		}))
		response := httptest.NewRecorder()
		server.handleMe(response, request)
		var user publicUser
		if err := json.Unmarshal(response.Body.Bytes(), &user); err != nil {
			t.Fatal(err)
		}
		if user.Admin != tc.want {
			t.Errorf("%s admin = %v, want %v", tc.username, user.Admin, tc.want)
		}
	}
}

// TestAdminSettingsRoundTrip covers reading and updating the shared settings.
func TestAdminSettingsRoundTrip(t *testing.T) {
	server := newTestServer(t)

	response := httptest.NewRecorder()
	server.handleGetSettings(response, httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil))
	var settings serverSettings
	if err := json.Unmarshal(response.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	if !settings.AllowRegistration || !settings.AllowUserKeys {
		t.Errorf("defaults = %+v, want both switches on", settings)
	}
	if settings.DefaultModel != "openrouter/free" {
		t.Errorf("default model = %q, want the flag value", settings.DefaultModel)
	}

	body := `{"defaultModel":"deepseek-chat","defaultThinking":"high","allowRegistration":false,"allowUserKeys":false}`
	update := httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(body))
	updateResponse := httptest.NewRecorder()
	server.handleUpdateSettings(updateResponse, update)
	if updateResponse.Code != http.StatusOK {
		t.Fatalf("update: %d %s", updateResponse.Code, updateResponse.Body.String())
	}
	if got := server.settings.get(); got.DefaultModel != "deepseek-chat" || got.AllowRegistration || got.AllowUserKeys {
		t.Errorf("settings = %+v", got)
	}

	// A blank model must not erase the configured one.
	blank := httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(`{"allowRegistration":true,"allowUserKeys":true}`))
	server.handleUpdateSettings(httptest.NewRecorder(), blank)
	if got := server.settings.get(); got.DefaultModel != "deepseek-chat" {
		t.Errorf("a blank model erased the setting: %+v", got)
	}

	// Settings survive a restart.
	reopened, err := newSettingsStore(server.db, server.config)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.get(); got.DefaultModel != "deepseek-chat" || got.DefaultThinking != "high" {
		t.Errorf("reloaded settings = %+v", got)
	}
}

// TestRegistrationSwitch verifies a closed deployment refuses signups while the
// service token still gets through.
func TestRegistrationSwitch(t *testing.T) {
	server := newTestServer(t)
	settings := server.settings.get()
	settings.AllowRegistration = false
	if _, err := server.settings.update(settings); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader(`{"username":"carol","password":"password-1234"}`))
	response := httptest.NewRecorder()
	server.handleRegister(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%s)", response.Code, response.Body.String())
	}

	// With a service token configured and presented, registration still works.
	server.config.token = "service-token"
	tokenRequest := httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader(`{"username":"carol","password":"password-1234"}`))
	tokenRequest.Header.Set("Authorization", "Bearer service-token")
	tokenResponse := httptest.NewRecorder()
	server.handleRegister(tokenResponse, tokenRequest)
	if tokenResponse.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", tokenResponse.Code, tokenResponse.Body.String())
	}
}

// TestAdminPublicCredentials covers the shared pool's HTTP surface.
func TestAdminPublicCredentials(t *testing.T) {
	server := newTestServer(t)
	const key = "sk-public-pool-value"

	request := httptest.NewRequest(http.MethodPut, "/api/admin/credentials/deepseek", strings.NewReader(`{"key":"`+key+`"}`))
	request.SetPathValue("provider", "deepseek")
	response := httptest.NewRecorder()
	server.handleSetPublicCredential(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("set: %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), key) {
		t.Fatal("the response echoed the public key")
	}
	if got := server.credentials.resolve("", "deepseek", true); got != key {
		t.Errorf("resolve = %q", got)
	}

	deleteRequest := httptest.NewRequest(http.MethodDelete, "/api/admin/credentials/deepseek", nil)
	deleteRequest.SetPathValue("provider", "deepseek")
	deleteResponse := httptest.NewRecorder()
	server.handleDeletePublicCredential(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", deleteResponse.Code, deleteResponse.Body.String())
	}
	if server.credentials.has("", "deepseek") {
		t.Error("the public key survived deletion")
	}

	// Deleting what is not there is a 404, not a silent success.
	missing := httptest.NewRequest(http.MethodDelete, "/api/admin/credentials/groq", nil)
	missing.SetPathValue("provider", "groq")
	missingResponse := httptest.NewRecorder()
	server.handleDeletePublicCredential(missingResponse, missing)
	if missingResponse.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", missingResponse.Code)
	}
}

// TestPublicModelsAreSharedAndDoNotExpire covers the shared catalog: everyone
// sees it, it has no expiry, and it survives deleting the account that happens
// to have the same model id.
func TestPublicModelsAreSharedAndDoNotExpire(t *testing.T) {
	server := newTestServer(t)

	request := adminRequest(t, http.MethodPost, "/api/admin/models",
		`{"id":"deepseek-chat","label":"DeepSeek Chat","provider":"deepseek"}`)
	response := httptest.NewRecorder()
	server.handleAddPublicModel(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("add: %d %s", response.Code, response.Body.String())
	}
	var created customModelResponse
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Scope != modelScopePublic || created.ExpiresAt != "" || created.Expired {
		t.Fatalf("created = %+v, want a public entry with no expiry", created)
	}

	// Every user sees it, including one who has added nothing.
	for _, userID := range []string{"user-1", "user-2"} {
		visible := server.visibleCustomModels(userRequest(t, http.MethodGet, "/api/models", "", userID, "u", false))
		found := false
		for _, m := range visible {
			if m.ID == "deepseek-chat" {
				found = true
				if strings.Contains(m.Label, "自定义") {
					t.Errorf("a shared entry must not be labelled as personal: %q", m.Label)
				}
			}
		}
		if !found {
			t.Errorf("%s cannot see the shared model", userID)
		}
	}

	// A user's own entry with the same id does not shadow the shared routing.
	if err := server.customModels.add(customModel{
		ID: "deepseek-chat", Label: "Mine", Provider: "openrouter", UserID: "user-1",
		ExpiresAt: time.Now().UTC().AddDate(0, 0, 3).Format("2006-01-02"),
	}); err != nil {
		t.Fatal(err)
	}
	visible := server.visibleCustomModels(userRequest(t, http.MethodGet, "/api/models", "", "user-1", "u", false))
	count := 0
	for _, m := range visible {
		if m.ID == "deepseek-chat" {
			count++
			if m.Provider != "deepseek" {
				t.Errorf("provider = %q, want the shared entry to win", m.Provider)
			}
		}
	}
	if count != 1 {
		t.Errorf("the model appears %d times, want it deduplicated", count)
	}

	// Deleting it is scoped: the user's entry stays.
	deleteRequest := adminRequest(t, http.MethodDelete, "/api/admin/models/deepseek-chat", "")
	deleteRequest.SetPathValue("id", "deepseek-chat")
	deleteResponse := httptest.NewRecorder()
	server.handleDeletePublicModel(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", deleteResponse.Code, deleteResponse.Body.String())
	}
	if len(server.customModels.listPublic()) != 0 {
		t.Error("the shared entry survived deletion")
	}
	if len(server.customModels.list("user-1")) != 1 {
		t.Error("deleting a shared entry removed the user's own")
	}
}

// TestPublicModelRejectsExpiry verifies the shared catalog ignores a date even
// if a client sends one, so entries cannot silently stop working.
func TestPublicModelRejectsExpiry(t *testing.T) {
	server := newTestServer(t)
	request := adminRequest(t, http.MethodPost, "/api/admin/models",
		`{"id":"deepseek-chat","provider":"deepseek","expiresAt":"2020-01-01"}`)
	response := httptest.NewRecorder()
	server.handleAddPublicModel(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("add: %d %s", response.Code, response.Body.String())
	}
	models := server.customModels.listPublic()
	if len(models) != 1 || models[0].ExpiresAt != "" {
		t.Fatalf("stored = %+v, want the date dropped", models)
	}
	if models[0].expired(time.Now().UTC()) {
		t.Error("a shared entry must never be expired")
	}
}

// TestCustomModelScopeMigration verifies entries written before scopes existed
// are imported as user entries rather than becoming shared.
func TestCustomModelScopeMigration(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "models"), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := `[{"id":"legacy/model","label":"Legacy","provider":"openrouter","userId":"user-1","expiresAt":"2099-01-01","createdAt":"2026-01-01T00:00:00Z"}]`
	if err := os.WriteFile(filepath.Join(dir, "models", "models.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	db := openTestDB(t)
	if _, err := importLegacy(dir, db); err != nil {
		t.Fatal(err)
	}
	store, err := newCustomModelStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.listPublic(); len(got) != 0 {
		t.Fatalf("a legacy entry became public: %+v", got)
	}
	own := store.list("user-1")
	if len(own) != 1 || own[0].Scope != modelScopeUser {
		t.Fatalf("legacy entry = %+v, want scope %q", own, modelScopeUser)
	}
}

// TestCustomProviderValidation covers the rules that keep a custom name usable
// as a credential key and distinguishable from a built-in.
func TestCustomProviderValidation(t *testing.T) {
	ok := []customProvider{
		{Name: "my-gateway", Protocol: "openai", BaseURL: "http://10.0.0.5:8000/v1"},
		{Name: "vllm_1", Protocol: "anthropic", BaseURL: "https://gw.example.com"},
		{Name: "MixedCase", Protocol: "OpenAI", BaseURL: "https://x.test/v1"},
	}
	for _, c := range ok {
		if _, err := c.validate(); err != nil {
			t.Errorf("%+v should be valid: %v", c, err)
		}
	}
	bad := []customProvider{
		{Name: "a", Protocol: "openai", BaseURL: "https://x.test"},          // too short
		{Name: "my gateway", Protocol: "openai", BaseURL: "https://x.test"}, // space
		{Name: "openai", Protocol: "openai", BaseURL: "https://x.test"},     // collides with a built-in
		{Name: "deepseek", Protocol: "openai", BaseURL: "https://x.test"},   // ditto
		{Name: "gw", Protocol: "grpc", BaseURL: "https://x.test"},           // unknown protocol
		{Name: "gw", Protocol: "openai", BaseURL: "10.0.0.5:8000"},          // no scheme
		{Name: "gw", Protocol: "openai", BaseURL: "file:///etc/passwd"},     // not http(s)
	}
	for _, c := range bad {
		if _, err := c.validate(); err == nil {
			t.Errorf("%+v should be rejected", c)
		}
	}
	// Normalization lowercases both fields.
	got, err := customProvider{Name: "MixedCase", Protocol: "OpenAI", BaseURL: " https://x.test/v1 "}.validate()
	if err != nil || got.Name != "mixedcase" || got.Protocol != "openai" || got.BaseURL != "https://x.test/v1" {
		t.Errorf("normalized = %+v, err = %v", got, err)
	}
}

// TestCustomProviderKeepsItsName is the point of the whole feature: a custom
// endpoint must not be filed under the generic driver identity, or its key
// would collide with the built-in provider of the same protocol.
func TestCustomProviderKeepsItsName(t *testing.T) {
	server := newTestServer(t)
	custom := customProvider{Name: "my-gateway", Protocol: "openai", BaseURL: "http://10.0.0.5:8000/v1"}
	if err := server.settings.putCustomProvider(custom); err != nil {
		t.Fatal(err)
	}

	prov, name, err := server.resolveProvider("some-model", "my-gateway")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if name != "my-gateway" {
		t.Fatalf("resolved name = %q, want the custom name (the driver reports %q)", name, prov.Name())
	}

	// Keys for the endpoint and for the built-in OpenAI provider stay separate.
	if err := server.credentials.setPublicKey("my-gateway", "sk-gateway"); err != nil {
		t.Fatal(err)
	}
	if err := server.credentials.setPublicKey("openai", "sk-real-openai"); err != nil {
		t.Fatal(err)
	}
	if got := server.credentials.resolve("", "my-gateway", true); got != "sk-gateway" {
		t.Errorf("gateway key = %q", got)
	}
	if got := server.credentials.resolve("", "openai", true); got != "sk-real-openai" {
		t.Errorf("openai key = %q", got)
	}

	// A built-in still resolves through the registry.
	if _, name, err := server.resolveProvider("deepseek-chat", "deepseek"); err != nil || name != "deepseek" {
		t.Errorf("built-in resolve = %q, %v", name, err)
	}
}

// TestCustomProviderAcceptsKeys verifies knownProvider lets a custom name
// through, which is what allows a key to be filed against it at all.
func TestCustomProviderAcceptsKeys(t *testing.T) {
	server := newTestServer(t)
	if server.knownProvider("my-gateway") {
		t.Fatal("an undefined name must not be accepted")
	}
	if err := server.settings.putCustomProvider(customProvider{
		Name: "my-gateway", Protocol: "openai", BaseURL: "http://10.0.0.5:8000/v1",
	}); err != nil {
		t.Fatal(err)
	}
	if !server.knownProvider("my-gateway") {
		t.Error("a defined custom provider must be accepted")
	}
	if !server.knownProvider("deepseek") {
		t.Error("built-ins must still be accepted")
	}
}

// TestDeleteCustomProviderDropsItsKey verifies removing an endpoint does not
// strand ciphertext nothing can reach.
func TestDeleteCustomProviderDropsItsKey(t *testing.T) {
	server := newTestServer(t)
	if err := server.settings.putCustomProvider(customProvider{
		Name: "my-gateway", Protocol: "openai", BaseURL: "http://10.0.0.5:8000/v1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := server.credentials.setPublicKey("my-gateway", "sk-gateway"); err != nil {
		t.Fatal(err)
	}

	request := adminRequest(t, http.MethodDelete, "/api/admin/providers/my-gateway", "")
	request.SetPathValue("name", "my-gateway")
	response := httptest.NewRecorder()
	server.handleDeleteCustomProvider(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
	if _, ok := server.settings.findCustomProvider("my-gateway"); ok {
		t.Error("the endpoint survived")
	}
	if server.credentials.has("", "my-gateway") {
		t.Error("its public key survived")
	}
}

// TestProvidersListIncludesCustom verifies the endpoint shows up in the list the
// UI builds its provider table from.
func TestProvidersListIncludesCustom(t *testing.T) {
	server := newTestServer(t)
	if err := server.settings.putCustomProvider(customProvider{
		Name: "my-gateway", Protocol: "openai", BaseURL: "http://10.0.0.5:8000/v1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := server.credentials.setUserKey("user-1", "my-gateway", "sk-mine"); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	server.handleListProviders(response, userRequest(t, http.MethodGet, "/api/providers", "", "user-1", "alice", false))
	var providers []providerInfoResponse
	if err := json.Unmarshal(response.Body.Bytes(), &providers); err != nil {
		t.Fatal(err)
	}
	var found *providerInfoResponse
	for i := range providers {
		if providers[i].Name == "my-gateway" {
			found = &providers[i]
		}
	}
	if found == nil {
		t.Fatal("the custom provider is missing from /api/providers")
	}
	if !found.Custom || found.Protocol != "openai" || found.BaseURL != "http://10.0.0.5:8000/v1" {
		t.Errorf("entry = %+v", *found)
	}
	if found.Source != "user" {
		t.Errorf("source = %q, want user", found.Source)
	}
}

// TestCustomProviderReachesEveryResolutionPath guards the three places a
// provider name is validated. Fixing only the agent loop left "添加公共模型"
// rejecting a perfectly good custom endpoint with "unknown --provider", because
// each path called provider.ResolveProvider directly.
func TestCustomProviderReachesEveryResolutionPath(t *testing.T) {
	server := newTestServer(t)
	if err := server.settings.putCustomProvider(customProvider{
		Name: "bonai", Protocol: "openai", BaseURL: "http://192.168.50.41/v1",
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("公共模型", func(t *testing.T) {
		request := adminRequest(t, http.MethodPost, "/api/admin/models",
			`{"id":"bonsai2","label":"Bonsai","provider":"bonai"}`)
		response := httptest.NewRecorder()
		server.handleAddPublicModel(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d %s", response.Code, response.Body.String())
		}
	})

	t.Run("个人模型", func(t *testing.T) {
		request := userRequest(t, http.MethodPost, "/api/custom-models",
			`{"id":"bonsai3","provider":"bonai","expiresAt":"2099-01-01"}`, "user-1", "alice", false)
		response := httptest.NewRecorder()
		server.handleAddCustomModel(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d %s", response.Code, response.Body.String())
		}
	})

	t.Run("斜杠命令切换模型", func(t *testing.T) {
		meta := &sessionMeta{Model: "openrouter/free", Provider: "openrouter"}
		if _, err := applyModelForProvider(meta, "bonsai2", "bonai", server.resolveProvider); err != nil {
			t.Fatalf("switch: %v", err)
		}
		if meta.Provider != "bonai" {
			t.Errorf("provider = %q, want the custom name preserved", meta.Provider)
		}
	})

	t.Run("未定义的名字仍被拒绝", func(t *testing.T) {
		request := adminRequest(t, http.MethodPost, "/api/admin/models",
			`{"id":"x","provider":"not-configured"}`)
		response := httptest.NewRecorder()
		server.handleAddPublicModel(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", response.Code)
		}
	})
}

// TestNewSessionUsesAdminDefaults verifies the 部署 panel's default model is
// what new sessions actually start with. It used to read the start-up flag, so
// changing the default in the UI silently did nothing.
func TestNewSessionUsesAdminDefaults(t *testing.T) {
	server := newTestServer(t)
	if err := server.settings.putCustomProvider(customProvider{
		Name: "bonai", Protocol: "openai", BaseURL: "http://192.168.50.41/v1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := server.customModels.add(customModel{
		ID: "bonsai2", Label: "Bonsai", Provider: "bonai", Scope: modelScopePublic,
	}); err != nil {
		t.Fatal(err)
	}
	settings := server.settings.get()
	settings.DefaultModel = "bonsai2"
	settings.DefaultThinking = "high"
	if _, err := server.settings.update(settings); err != nil {
		t.Fatal(err)
	}

	id := createSession(t, server)
	server.mu.RLock()
	managed := server.sessions[id]
	server.mu.RUnlock()
	if managed == nil {
		t.Fatal("session not registered")
	}
	if managed.meta.Model != "bonsai2" || managed.meta.Thinking != "high" {
		t.Errorf("session = %+v, want the administrator's defaults", managed.meta)
	}
	// The catalog entry names the endpoint; falling back to the heuristics here
	// would land on openrouter.
	if managed.meta.Provider != "bonai" {
		t.Errorf("provider = %q, want bonai", managed.meta.Provider)
	}
}

// TestModelListingShowsOnlyUsableModels pins what /models means in the web:
// custom models, OpenRouter's free catalog, and presets for keyed providers.
func TestModelListingShowsOnlyUsableModels(t *testing.T) {
	server := newTestServer(t)
	clearProviderKeys(t)
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"vendor/free-model:free","name":"Free Model","architecture":{"output_modalities":["text"]},"pricing":{"prompt":"0","completion":"0"}}]}`))
	}))
	defer catalog.Close()
	server.openRouterModelsURL = catalog.URL
	server.modelHTTPClient = catalog.Client()

	if err := server.settings.putCustomProvider(customProvider{
		Name: "bonai", Protocol: "openai", BaseURL: "http://192.168.50.41/v1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := server.customModels.add(customModel{
		ID: "bonsai2", Label: "Bonsai", Provider: "bonai", Scope: modelScopePublic,
	}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)

	listing := server.modelListing(request, "user-1", "")
	if !strings.Contains(listing, "bonsai2") {
		t.Errorf("custom model missing:\n%s", listing)
	}
	if !strings.Contains(listing, "vendor/free-model:free") {
		t.Errorf("OpenRouter free model missing:\n%s", listing)
	}
	// Static OpenRouter presets (mostly paid) never show, key or not.
	t.Setenv("OPENROUTER_API_KEY", "sk-env")
	listing = server.modelListing(request, "user-1", "")
	if strings.Contains(listing, "openai/gpt-4o") {
		t.Errorf("static OpenRouter presets must not be listed:\n%s", listing)
	}
	// No key: no deepseek presets.
	if strings.Contains(listing, "deepseek-v4-pro") {
		t.Errorf("preset listed without a key:\n%s", listing)
	}
	// A key makes that provider's presets appear, for that user only.
	if err := server.credentials.setUserKey("user-1", "deepseek", "sk-x"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(server.modelListing(request, "user-1", ""), "deepseek-v4-pro") {
		t.Error("deepseek presets should appear once it has a key")
	}
	if strings.Contains(server.modelListing(request, "user-2", ""), "deepseek-v4-pro") {
		t.Error("a personal key leaked into another user's listing")
	}
}

// TestSlashRepliesKeepTheirLines guards the regression where every multi-line
// command reply collapsed into one paragraph once assistant bubbles started
// rendering Markdown (a lone newline is only a soft break there).
func TestSlashRepliesKeepTheirLines(t *testing.T) {
	if got := asMarkdownLines("a\n  b\nc"); got != "a  \nb  \nc" {
		t.Errorf("asMarkdownLines = %q", got)
	}
	if asMarkdownLines("") != "" {
		t.Error("an empty reply must stay empty")
	}
	_, message, complete, err := resolveWebInput(&sessionMeta{}, "/help", builtinResolver, presetListing)
	if err != nil || !complete {
		t.Fatalf("/help: complete=%v err=%v", complete, err)
	}
	if strings.Count(message, "  \n") < 2 {
		t.Errorf("/help lost its line breaks: %q", message)
	}
}

// TestSlashModelUsesTheCatalogProvider is the "/model bonai2 → provider:
// openrouter" bug: with no provider named, the switch must take the provider the
// catalog entry declares, or the session sends the model to the wrong server.
func TestSlashModelUsesTheCatalogProvider(t *testing.T) {
	server := newTestServer(t)
	if err := server.settings.putCustomProvider(customProvider{
		Name: "bonai", Protocol: "openai", BaseURL: "http://192.168.50.41/v1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := server.customModels.add(customModel{
		ID: "bonai2", Label: "bonai2", Provider: "bonai", Scope: modelScopePublic,
	}); err != nil {
		t.Fatal(err)
	}
	if err := server.customModels.add(customModel{
		ID: "mine-only", Label: "m", Provider: "bonai", UserID: "user-1",
		ExpiresAt: time.Now().UTC().AddDate(0, 0, 3).Format("2006-01-02"),
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ user, model, want string }{
		{"user-1", "bonai2", "bonai"},           // shared catalog entry
		{"user-1", "mine-only", "bonai"},        // the user's own entry
		{"user-2", "deepseek-chat", "deepseek"}, // not in the catalog: registry as before
	} {
		meta := &sessionMeta{Model: "openrouter/free", Provider: "openrouter"}
		_, reply, _, err := resolveWebInput(meta, "/model "+tc.model, server.resolverFor(tc.user), presetListing)
		if err != nil {
			t.Fatalf("/model %s: %v", tc.model, err)
		}
		if meta.Provider != tc.want {
			t.Errorf("/model %s as %s → provider %q, want %q (reply %q)", tc.model, tc.user, meta.Provider, tc.want, reply)
		}
	}
}

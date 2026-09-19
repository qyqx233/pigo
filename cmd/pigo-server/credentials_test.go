package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testSecret = "test-master-secret"

func newStore(t *testing.T, dir, secret string) *credentialStore {
	t.Helper()
	store, err := newCredentialStore(dir, secret)
	if err != nil {
		t.Fatalf("open credential store: %v", err)
	}
	return store
}

// TestCredentialRoundTrip covers the ordinary lifecycle of one key.
func TestCredentialRoundTrip(t *testing.T) {
	store := newStore(t, t.TempDir(), testSecret)

	if err := store.setUserKey("user-1", "deepseek", "sk-secret-value-1234"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := store.resolve("user-1", "deepseek", true); got != "sk-secret-value-1234" {
		t.Errorf("resolve = %q", got)
	}
	if got := store.hint("user-1", "deepseek"); got != "••••1234" {
		t.Errorf("hint = %q, want the last four characters masked", got)
	}
	if got := store.configuredProviders("user-1"); len(got) != 1 || got[0] != "deepseek" {
		t.Errorf("configuredProviders = %v", got)
	}
	// Provider names are normalized, so the UI's casing cannot create a second
	// slot that shadows the first.
	if got := store.resolve("user-1", "DeepSeek", true); got != "sk-secret-value-1234" {
		t.Errorf("provider lookup must be case-insensitive, got %q", got)
	}

	removed, err := store.deleteUserKey("user-1", "deepseek")
	if err != nil || !removed {
		t.Fatalf("delete = %v, %v", removed, err)
	}
	if got := store.resolve("user-1", "deepseek", true); got != "" {
		t.Errorf("resolve after delete = %q", got)
	}
}

// TestCredentialCiphertextOnDisk is the core security assertion: the key must
// not be recoverable from the data directory alone.
func TestCredentialCiphertextOnDisk(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t, dir, testSecret)
	const key = "sk-plaintext-must-not-appear"
	if err := store.setUserKey("user-1", "openai", key); err != nil {
		t.Fatal(err)
	}
	if err := store.setPublicKey("openai", key+"-public"); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), key) {
		t.Fatal("credentials.json contains the key in plaintext")
	}
	if !strings.Contains(string(raw), "openai") {
		t.Fatal("provider names are expected to stay readable; the file shape changed")
	}
}

// TestCredentialWrongSecret verifies a store opened with a different master
// secret degrades to "nothing configured" instead of returning garbage or
// panicking.
func TestCredentialWrongSecret(t *testing.T) {
	dir := t.TempDir()
	if err := newStore(t, dir, testSecret).setUserKey("user-1", "openai", "sk-original"); err != nil {
		t.Fatal(err)
	}

	other := newStore(t, dir, "a-different-secret")
	if got := other.resolve("user-1", "openai", true); got != "" {
		t.Errorf("resolve with the wrong secret = %q, want empty", got)
	}
	// The slot still reports as present — the record exists, it just cannot be
	// opened — which is what lets the user replace it.
	if !other.has("user-1", "openai") {
		t.Error("the record should still be listed")
	}
	if got := other.hint("user-1", "openai"); got != "" {
		t.Errorf("hint with the wrong secret = %q", got)
	}
}

// TestCredentialSlotBinding verifies the additional data really binds a record
// to its slot: a ciphertext moved from one user to another, or into the public
// pool, must not open.
func TestCredentialSlotBinding(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t, dir, testSecret)
	if err := store.setUserKey("user-1", "openai", "sk-user-one"); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state credentialState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	record := state.Users["user-1"]["openai"]
	if record == "" {
		t.Fatal("no record written")
	}

	// Move the same ciphertext into three other slots by hand.
	state.Users["user-2"] = map[string]string{"openai": record}
	state.Public = map[string]string{"openai": record}
	state.Users["user-1"]["anthropic"] = record
	moved, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), moved, 0o600); err != nil {
		t.Fatal(err)
	}

	tampered := newStore(t, dir, testSecret)
	if got := tampered.resolve("user-2", "openai", true); got != "" {
		t.Errorf("a record moved to another user opened: %q", got)
	}
	if got := tampered.resolve("user-1", "anthropic", true); got != "" {
		t.Errorf("a record moved to another provider opened: %q", got)
	}
	// The original slot still works.
	if got := tampered.resolve("user-1", "openai", true); got != "sk-user-one" {
		t.Errorf("the original slot must still open, got %q", got)
	}
}

// TestCredentialDisabled verifies the degradation contract: writes are refused
// with a reason, reads report nothing, and an existing file survives untouched
// so setting the secret later restores the data.
func TestCredentialDisabled(t *testing.T) {
	dir := t.TempDir()
	if err := newStore(t, dir, testSecret).setUserKey("user-1", "openai", "sk-survives"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}

	disabled := newStore(t, dir, "")
	if disabled.enabled() {
		t.Fatal("a store with no secret must report disabled")
	}
	if err := disabled.setUserKey("user-1", "openai", "sk-new"); err == nil {
		t.Error("writing to a disabled store must fail")
	} else if !strings.Contains(err.Error(), credentialSecretEnv) {
		t.Errorf("the error must name the variable to set, got %v", err)
	}
	if got := disabled.resolve("user-1", "openai", true); got != "" {
		t.Errorf("resolve = %q, want empty", got)
	}
	if got := disabled.configuredProviders("user-1"); got != nil {
		t.Errorf("configuredProviders = %v, want nil", got)
	}
	// dropUser is called during account deletion and must not fail the delete
	// just because keys cannot be read.
	if err := disabled.dropUser("user-1"); err != nil {
		t.Errorf("dropUser on a disabled store = %v", err)
	}

	after, err := os.ReadFile(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("a disabled store must not rewrite credentials.json")
	}
	if got := newStore(t, dir, testSecret).resolve("user-1", "openai", true); got != "sk-survives" {
		t.Errorf("data must survive a restart without the secret, got %q", got)
	}
}

// TestCredentialPrecedence pins the tier order the spec promises.
func TestCredentialPrecedence(t *testing.T) {
	store := newStore(t, t.TempDir(), testSecret)
	if err := store.setPublicKey("openai", "sk-public"); err != nil {
		t.Fatal(err)
	}

	// No personal key: the shared pool applies.
	if got := store.resolve("user-1", "openai", true); got != "sk-public" {
		t.Errorf("resolve = %q, want the public key", got)
	}
	// A personal key wins.
	if err := store.setUserKey("user-1", "openai", "sk-mine"); err != nil {
		t.Fatal(err)
	}
	if got := store.resolve("user-1", "openai", true); got != "sk-mine" {
		t.Errorf("resolve = %q, want the user's own key", got)
	}
	// With personal keys switched off the stored key is ignored, not deleted.
	if got := store.resolve("user-1", "openai", false); got != "sk-public" {
		t.Errorf("resolve with user keys disabled = %q, want the public key", got)
	}
	if got := store.resolve("user-1", "openai", true); got != "sk-mine" {
		t.Error("switching the flag back must restore the user's key")
	}
	// An anonymous/service principal never picks up someone else's key.
	if got := store.resolve("", "openai", true); got != "sk-public" {
		t.Errorf("resolve with no user = %q", got)
	}
	// Nothing stored for a provider leaves the environment fallback in place.
	if got := store.resolve("user-1", "groq", true); got != "" {
		t.Errorf("resolve for an unconfigured provider = %q, want empty", got)
	}
}

// TestCredentialDropUser covers the account-deletion path.
func TestCredentialDropUser(t *testing.T) {
	store := newStore(t, t.TempDir(), testSecret)
	for _, name := range []string{"openai", "groq"} {
		if err := store.setUserKey("user-1", name, "sk-"+name); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.setPublicKey("openai", "sk-public"); err != nil {
		t.Fatal(err)
	}
	if err := store.dropUser("user-1"); err != nil {
		t.Fatal(err)
	}
	if got := store.configuredProviders("user-1"); len(got) != 0 {
		t.Errorf("user keys after drop = %v", got)
	}
	if got := store.resolve("user-1", "openai", true); got != "sk-public" {
		t.Errorf("the shared pool must survive dropping a user, got %q", got)
	}
}

// TestCredentialRejectsBadInput covers the validation on the write path.
func TestCredentialRejectsBadInput(t *testing.T) {
	store := newStore(t, t.TempDir(), testSecret)
	if err := store.setUserKey("user-1", "openai", "   "); err == nil {
		t.Error("a blank key must be rejected")
	}
	if err := store.setUserKey("user-1", "", "sk-x"); err == nil {
		t.Error("a missing provider must be rejected")
	}
	if err := store.setUserKey("user-1", "openai", strings.Repeat("x", maxAPIKeyLength+1)); err == nil {
		t.Error("an oversized key must be rejected")
	}
}

// --- HTTP surface -----------------------------------------------------------

// userRequest builds a request carrying a user principal, as requirePrincipal
// would produce for a signed-in browser.
func userRequest(t *testing.T, method, target, body, userID, username string, admin bool) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	return request.WithContext(withPrincipal(request, requestPrincipal{
		UserID:   userID,
		Username: username,
		Admin:    admin,
	}))
}

// TestCredentialAPINeverReturnsTheKey is the HTTP-level counterpart to the
// on-disk assertion: no response body may contain the stored key.
func TestCredentialAPINeverReturnsTheKey(t *testing.T) {
	server := newTestServer(t)
	const key = "sk-never-echo-this-value"

	request := userRequest(t, http.MethodPut, "/api/credentials/openai", `{"key":"`+key+`"}`, "user-1", "alice", false)
	request.SetPathValue("provider", "openai")
	response := httptest.NewRecorder()
	server.handleSetCredential(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("set credential: %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), key) {
		t.Fatal("the write response echoed the key")
	}

	listRequest := userRequest(t, http.MethodGet, "/api/credentials", "", "user-1", "alice", false)
	listResponse := httptest.NewRecorder()
	server.handleListCredentials(listResponse, listRequest)
	if strings.Contains(listResponse.Body.String(), key) {
		t.Fatal("the list response echoed the key")
	}
	var payload credentialsEnabledResponse
	if err := json.Unmarshal(listResponse.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Enabled || len(payload.Credentials) != 1 {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Credentials[0].Provider != "openai" || payload.Credentials[0].Hint != "••••alue" {
		t.Errorf("credential = %+v", payload.Credentials[0])
	}
}

// TestCredentialAPIIsolatesUsers verifies one user cannot see another's keys.
func TestCredentialAPIIsolatesUsers(t *testing.T) {
	server := newTestServer(t)
	if err := server.credentials.setUserKey("user-1", "openai", "sk-alice"); err != nil {
		t.Fatal(err)
	}

	request := userRequest(t, http.MethodGet, "/api/credentials", "", "user-2", "bob", false)
	response := httptest.NewRecorder()
	server.handleListCredentials(response, request)
	var payload credentialsEnabledResponse
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Credentials) != 0 {
		t.Fatalf("bob sees alice's credentials: %+v", payload.Credentials)
	}
}

// TestCredentialAPIRejectsUnknownProvider keeps typos out of the store, where
// they would look configured and never be used.
func TestCredentialAPIRejectsUnknownProvider(t *testing.T) {
	server := newTestServer(t)
	request := userRequest(t, http.MethodPut, "/api/credentials/nope", `{"key":"sk-x"}`, "user-1", "alice", false)
	request.SetPathValue("provider", "nope")
	response := httptest.NewRecorder()
	server.handleSetCredential(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
}

// TestCredentialAPIHonorsTheUserKeySwitch verifies the administrator's switch
// blocks new personal keys.
func TestCredentialAPIHonorsTheUserKeySwitch(t *testing.T) {
	server := newTestServer(t)
	settings := server.settings.get()
	settings.AllowUserKeys = false
	if _, err := server.settings.update(settings); err != nil {
		t.Fatal(err)
	}

	request := userRequest(t, http.MethodPut, "/api/credentials/openai", `{"key":"sk-x"}`, "user-1", "alice", false)
	request.SetPathValue("provider", "openai")
	response := httptest.NewRecorder()
	server.handleSetCredential(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.Code)
	}
}

// TestCredentialAPIDisabledReportsReason verifies a server with no master
// secret says so instead of silently dropping the key.
func TestCredentialAPIDisabledReportsReason(t *testing.T) {
	server := newTestServer(t)
	server.credentials = newStore(t, t.TempDir(), "")

	request := userRequest(t, http.MethodPut, "/api/credentials/openai", `{"key":"sk-x"}`, "user-1", "alice", false)
	request.SetPathValue("provider", "openai")
	response := httptest.NewRecorder()
	server.handleSetCredential(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
	if !strings.Contains(response.Body.String(), credentialSecretEnv) {
		t.Errorf("the reason must name the variable: %s", response.Body.String())
	}

	listResponse := httptest.NewRecorder()
	server.handleListCredentials(listResponse, userRequest(t, http.MethodGet, "/api/credentials", "", "user-1", "alice", false))
	var payload credentialsEnabledResponse
	if err := json.Unmarshal(listResponse.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Enabled || payload.Reason == "" {
		t.Errorf("payload = %+v, want disabled with a reason", payload)
	}
}

// TestProvidersReportCredentialSource verifies /api/providers names the tier a
// run would really use.
func TestProvidersReportCredentialSource(t *testing.T) {
	server := newTestServer(t)
	t.Setenv("GROQ_API_KEY", "sk-from-env")
	if err := server.credentials.setPublicKey("deepseek", "sk-public"); err != nil {
		t.Fatal(err)
	}
	if err := server.credentials.setUserKey("user-1", "openai", "sk-mine"); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	server.handleListProviders(response, userRequest(t, http.MethodGet, "/api/providers", "", "user-1", "alice", false))
	var providers []providerInfoResponse
	if err := json.Unmarshal(response.Body.Bytes(), &providers); err != nil {
		t.Fatal(err)
	}
	sources := map[string]string{}
	for _, item := range providers {
		sources[item.Name] = item.Source
	}
	for name, want := range map[string]string{
		"openai":   "user",
		"deepseek": "public",
		"groq":     "env",
		"mistral":  "none",
	} {
		if sources[name] != want {
			t.Errorf("source[%s] = %q, want %q", name, sources[name], want)
		}
	}
}

package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// registerUser creates an account directly through the store and returns its id.
func registerUser(t *testing.T, server *apiServer, username string) string {
	t.Helper()
	user, _, _, err := server.auth.register(username, "password-1234")
	if err != nil {
		t.Fatalf("register %s: %v", username, err)
	}
	return user.ID
}

// adminRequest builds a request carrying an administrator principal.
func adminRequest(t *testing.T, method, target, body string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	return request.WithContext(withPrincipal(request, requestPrincipal{
		UserID:   "admin-id",
		Username: "alice",
		Admin:    true,
	}))
}

// TestDisabledUserCannotSignIn covers both doors: a fresh login and a cookie
// that was already issued.
func TestDisabledUserCannotSignIn(t *testing.T) {
	server := newTestServer(t)
	id := registerUser(t, server, "bob")

	_, token, err := server.auth.login("bob", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := server.auth.authenticate(token); !ok {
		t.Fatal("the session should be valid before suspension")
	}

	if _, err := server.auth.setDisabled(id, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := server.auth.login("bob", "password-1234"); err == nil {
		t.Error("a suspended account must not be able to sign in")
	}
	if _, ok := server.auth.authenticate(token); ok {
		t.Error("suspension must revoke the account's existing sessions")
	}

	// Restoring the account lets it sign in again; the old token stays dead.
	if _, err := server.auth.setDisabled(id, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := server.auth.login("bob", "password-1234"); err != nil {
		t.Errorf("a restored account must be able to sign in: %v", err)
	}
	if _, ok := server.auth.authenticate(token); ok {
		t.Error("a revoked token must stay revoked")
	}
}

// TestResetPassword verifies the temporary password works exactly once as a
// replacement and that the old one stops working.
func TestResetPassword(t *testing.T) {
	server := newTestServer(t)
	id := registerUser(t, server, "bob")
	_, token, err := server.auth.login("bob", "password-1234")
	if err != nil {
		t.Fatal(err)
	}

	temporary, err := server.auth.resetPassword(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(temporary) != 16 {
		t.Errorf("temporary password = %q, want 16 characters", temporary)
	}
	if _, _, err := server.auth.login("bob", "password-1234"); err == nil {
		t.Error("the old password must stop working")
	}
	if _, _, err := server.auth.login("bob", temporary); err != nil {
		t.Errorf("the temporary password must work: %v", err)
	}
	if _, ok := server.auth.authenticate(token); ok {
		t.Error("a password reset must revoke existing sessions")
	}

	// The plaintext is not recoverable from the store.
	var hashes strings.Builder
	if err := server.db.query("SELECT password_hash FROM users", func(rows *sql.Rows) error {
		var h string
		err := rows.Scan(&h)
		hashes.WriteString(h)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hashes.String(), temporary) {
		t.Fatal("the users table holds the temporary password in plaintext")
	}
}

// TestRandomPasswordAlphabet guards the generator against the confusable
// characters it deliberately excludes.
func TestRandomPasswordAlphabet(t *testing.T) {
	seen := map[rune]bool{}
	for range 50 {
		password, err := randomPassword()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range password {
			seen[r] = true
			if !strings.ContainsRune(passwordAlphabet, r) {
				t.Fatalf("password %q contains %q, which is outside the alphabet", password, r)
			}
		}
	}
	if len(seen) < 20 {
		t.Errorf("only %d distinct characters over 50 passwords; the generator looks biased", len(seen))
	}
}

// TestAdminCannotActOnSelf covers the three self-destructive operations.
func TestAdminCannotActOnSelf(t *testing.T) {
	server := newTestServer(t)
	id := registerUser(t, server, "alice")

	for _, tc := range []struct {
		name    string
		method  string
		body    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"disable", http.MethodPost, `{"disabled":true}`, server.handleSetUserDisabled},
		{"reset password", http.MethodPost, "", server.handleResetUserPassword},
		{"delete", http.MethodDelete, "", server.handleDeleteUser},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, "/api/admin/users/"+id, strings.NewReader(tc.body))
			request.SetPathValue("id", id)
			request = request.WithContext(withPrincipal(request, requestPrincipal{
				UserID: id, Username: "alice", Admin: true,
			}))
			response := httptest.NewRecorder()
			tc.handler(response, request)
			if response.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (%s)", response.Code, response.Body.String())
			}
		})
	}
	// The account survived all three attempts.
	if _, ok := server.auth.findUser(id); !ok {
		t.Fatal("the administrator's own account was removed")
	}
}

// TestDeleteUserCascade verifies every store an account touches is cleaned up,
// and that another account's data is left alone.
func TestDeleteUserCascade(t *testing.T) {
	server := newTestServer(t)
	victim := registerUser(t, server, "bob")
	bystander := registerUser(t, server, "carol")

	for _, id := range []string{victim, bystander} {
		if err := server.credentials.setUserKey(id, "openai", "sk-"+id); err != nil {
			t.Fatal(err)
		}
		if err := server.customModels.add(customModel{
			ID: "model-" + id, Label: "m", Provider: "openrouter", UserID: id,
			ExpiresAt: time.Now().UTC().AddDate(0, 0, 7).Format("2006-01-02"),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := server.customModels.add(customModel{
		ID: "shared-model", Label: "Shared", Provider: "openrouter", Scope: modelScopePublic,
	}); err != nil {
		t.Fatal(err)
	}

	request := adminRequest(t, http.MethodDelete, "/api/admin/users/"+victim, "")
	request.SetPathValue("id", victim)
	response := httptest.NewRecorder()
	server.handleDeleteUser(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
	var payload deleteUserResponse
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Problems) != 0 {
		t.Errorf("problems = %v, want a clean delete", payload.Problems)
	}

	if _, ok := server.auth.findUser(victim); ok {
		t.Error("the account survived")
	}
	if got := server.credentials.configuredProviders(victim); len(got) != 0 {
		t.Errorf("credentials survived: %v", got)
	}
	if got := server.customModels.list(victim); len(got) != 0 {
		t.Errorf("custom models survived: %+v", got)
	}

	// The bystander is untouched, and so is the shared catalog.
	if _, ok := server.auth.findUser(bystander); !ok {
		t.Error("deleting one account removed another")
	}
	if got := server.credentials.configuredProviders(bystander); len(got) != 1 {
		t.Errorf("bystander credentials = %v", got)
	}
	if got := server.customModels.list(bystander); len(got) != 1 {
		t.Errorf("bystander models = %+v", got)
	}
	if got := server.customModels.listPublic(); len(got) != 1 {
		t.Errorf("shared catalog = %+v, want it untouched", got)
	}
}

// TestListUsersReportsRoleAndKeys covers the console's read model.
func TestListUsersReportsRoleAndKeys(t *testing.T) {
	server := newTestServer(t)
	server.config.admins = parseAdminRoster("alice")
	aliceID := registerUser(t, server, "alice")
	bobID := registerUser(t, server, "bob")
	if err := server.credentials.setUserKey(bobID, "groq", "sk-bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := server.auth.setDisabled(bobID, true); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	server.handleListUsers(response, adminRequest(t, http.MethodGet, "/api/admin/users", ""))
	var users []adminUserResponse
	if err := json.Unmarshal(response.Body.Bytes(), &users); err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("users = %d, want 2", len(users))
	}
	byID := map[string]adminUserResponse{}
	for _, user := range users {
		byID[user.ID] = user
	}
	if !byID[aliceID].Admin || byID[aliceID].Disabled {
		t.Errorf("alice = %+v", byID[aliceID])
	}
	if byID[bobID].Admin || !byID[bobID].Disabled {
		t.Errorf("bob = %+v", byID[bobID])
	}
	if got := byID[bobID].Credentials; len(got) != 1 || got[0] != "groq" {
		t.Errorf("bob credentials = %v", got)
	}
	// The listing must not leak password material.
	if strings.Contains(response.Body.String(), "passwordHash") {
		t.Error("the user listing exposed password hashes")
	}
}

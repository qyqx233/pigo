package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// createPlainSession makes a session with no scene and returns it.
func createPlainSession(t *testing.T, server *apiServer) *managedSession {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	server.handleCreateSession(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", response.Code, response.Body.String())
	}
	var payload struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &payload)
	return server.sessions[payload.ID]
}

func newDraft(t *testing.T, server *apiServer) string {
	t.Helper()
	response := httptest.NewRecorder()
	server.handleCreateSession(response, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader("{}")))
	if response.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", response.Code, response.Body.String())
	}
	var payload struct {
		ID    string `json:"id"`
		Draft bool   `json:"draft"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &payload)
	if !payload.Draft {
		t.Fatalf("a new session is not a draft: %s", response.Body.String())
	}
	return payload.ID
}

func storedIDs(t *testing.T, server *apiServer) map[string]bool {
	t.Helper()
	stored, err := loadSessions(server.db, server.config.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for id := range stored {
		out[id] = true
	}
	return out
}

func listedIDs(t *testing.T, server *apiServer) string {
	t.Helper()
	response := httptest.NewRecorder()
	server.handleListSessions(response, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	return response.Body.String()
}

// TestDraftSessionWritesNothingUntilFirstMessage: 新会话 makes a session that
// works — it can be read, its settings changed — but leaves no directory and
// no row, and is not listed, until a message is sent in it.
func TestDraftSessionWritesNothingUntilFirstMessage(t *testing.T) {
	server := newTestServer(t)
	id := newDraft(t, server)
	paths := newSessionPaths(server.config.dataDir, id)

	get := func(target string, handler http.HandlerFunc) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.SetPathValue("id", id)
		response := httptest.NewRecorder()
		handler(response, request)
		return response
	}
	for target, handler := range map[string]http.HandlerFunc{
		"/api/sessions/" + id:               server.handleGetSession,
		"/api/sessions/" + id + "/messages": server.handleSessionMessages,
		"/api/sessions/" + id + "/commands": server.handleCommands,
		"/api/sessions/" + id + "/files":    server.handleListFiles,
	} {
		if response := get(target, handler); response.Code != http.StatusOK {
			t.Errorf("GET %s on a draft: %d %s", target, response.Code, response.Body.String())
		}
	}
	update := httptest.NewRequest(http.MethodPatch, "/api/sessions/"+id, strings.NewReader(`{"model":"openrouter/free","thinking":"high"}`))
	update.SetPathValue("id", id)
	response := httptest.NewRecorder()
	server.handleUpdateSession(response, update)
	if response.Code != http.StatusOK {
		t.Errorf("update a draft: %d %s", response.Code, response.Body.String())
	}

	if _, err := os.Stat(paths.Root); !os.IsNotExist(err) {
		t.Errorf("a draft has a directory: %v", err)
	}
	if storedIDs(t, server)[id] {
		t.Error("a draft is in the database")
	}
	if strings.Contains(listedIDs(t, server), id) {
		t.Error("a draft is listed")
	}

	sendPrompt(t, server, server.sessions[id], "hello", false)
	if _, err := os.Stat(paths.Workspace); err != nil {
		t.Errorf("the first message did not create the directory: %v", err)
	}
	if !storedIDs(t, server)[id] {
		t.Error("the first message did not store the session")
	}
	if list := listedIDs(t, server); !strings.Contains(list, id) || strings.Contains(list, `"draft"`) {
		t.Errorf("after the first message the list is %s", list)
	}
	if server.sessions[id].meta.Thinking != "high" {
		t.Error("the draft's settings were lost when it was stored")
	}
}

// TestDraftsPerUserAreBounded: each click makes a draft; a user keeps only the
// newest few, and stored sessions are never dropped.
func TestDraftsPerUserAreBounded(t *testing.T) {
	server := newTestServer(t)
	stored := createSession(t, server)
	var drafts []string
	for i := 0; i < maxDraftsPerUser+2; i++ {
		drafts = append(drafts, newDraft(t, server))
	}
	if _, ok := server.sessions[stored]; !ok {
		t.Fatal("a stored session was dropped")
	}
	kept := 0
	for i, id := range drafts {
		_, ok := server.sessions[id]
		if ok {
			kept++
		}
		if newest := i >= len(drafts)-maxDraftsPerUser; newest != ok {
			t.Errorf("draft %d kept = %v", i, ok)
		}
	}
	if kept != maxDraftsPerUser {
		t.Errorf("kept %d drafts, want %d", kept, maxDraftsPerUser)
	}
}

// TestReapAbandonedDraft: a draft has no workspace directory, and the reaper
// skips a session whose workspace it cannot read as empty — so without this an
// abandoned draft stayed in memory for the life of the process.
func TestReapAbandonedDraft(t *testing.T) {
	server := newTestServer(t)
	server.config.emptySessionTTL = time.Minute
	managed := createPlainSession(t, server)
	if !managed.meta.draft {
		t.Fatal("the new session is not a draft")
	}
	if _, err := os.Stat(managed.paths.Workspace); !os.IsNotExist(err) {
		t.Fatalf("a draft has a workspace directory: %v", err)
	}
	managed.meta.CreatedAt = time.Now().Add(-2 * time.Minute)
	managed.meta.LastUsed = managed.meta.CreatedAt

	server.reapIdle()
	if _, ok := server.getSession(managed.meta.ID); ok {
		t.Fatal("the abandoned draft was kept")
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
)

func TestRegisterLoginLogout(t *testing.T) {
	server := newTestServer(t)
	registerBody := `{"username":"alice","password":"correct-horse"}`
	registerRequest := httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader(registerBody))
	registerResponse := httptest.NewRecorder()
	server.handleRegister(registerResponse, registerRequest)
	if registerResponse.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", registerResponse.Code, registerResponse.Body.String())
	}
	cookies := registerResponse.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != authCookieName || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("auth cookie = %#v", cookies)
	}
	for _, file := range []string{server.db.target.path, server.db.target.path + "-wal"} {
		data, err := os.ReadFile(file)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("correct-horse")) {
			t.Fatalf("%s contains the plaintext password", filepath.Base(file))
		}
	}

	wrongRequest := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"username":"alice","password":"wrong-pass"}`))
	wrongResponse := httptest.NewRecorder()
	server.handleLogin(wrongResponse, wrongRequest)
	if wrongResponse.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", wrongResponse.Code)
	}

	meRequest := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	meRequest.AddCookie(cookies[0])
	meResponse := httptest.NewRecorder()
	server.requirePrincipal(http.HandlerFunc(server.handleMe)).ServeHTTP(meResponse, meRequest)
	if meResponse.Code != http.StatusOK || !strings.Contains(meResponse.Body.String(), `"username":"alice"`) {
		t.Fatalf("me: %d %s", meResponse.Code, meResponse.Body.String())
	}

	logoutRequest := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	logoutRequest.AddCookie(cookies[0])
	logoutResponse := httptest.NewRecorder()
	server.handleLogout(logoutResponse, logoutRequest)
	if logoutResponse.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", logoutResponse.Code)
	}
	meResponse = httptest.NewRecorder()
	server.requirePrincipal(http.HandlerFunc(server.handleMe)).ServeHTTP(meResponse, meRequest)
	if meResponse.Code != http.StatusUnauthorized {
		t.Fatalf("me after logout: %d", meResponse.Code)
	}
}

func TestEveryRegistrationRequiresConfiguredServerToken(t *testing.T) {
	server := newTestServer(t)
	server.config.token = "bootstrap-secret"
	body := `{"username":"alice","password":"correct-horse"}`
	unauthorized := httptest.NewRecorder()
	server.handleRegister(unauthorized, httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader(body)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("register without bootstrap token: %d %s", unauthorized.Code, unauthorized.Body.String())
	}

	request := httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer bootstrap-secret")
	response := httptest.NewRecorder()
	server.handleRegister(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("register with bootstrap token: %d %s", response.Code, response.Body.String())
	}

	secondBody := `{"username":"bob","password":"another-password"}`
	secondUnauthorized := httptest.NewRecorder()
	server.handleRegister(secondUnauthorized, httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader(secondBody)))
	if secondUnauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("second register without token: %d %s", secondUnauthorized.Code, secondUnauthorized.Body.String())
	}
	secondRequest := httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader(secondBody))
	secondRequest.Header.Set("Authorization", "Bearer bootstrap-secret")
	secondResponse := httptest.NewRecorder()
	server.handleRegister(secondResponse, secondRequest)
	if secondResponse.Code != http.StatusCreated {
		t.Fatalf("second register with token: %d %s", secondResponse.Code, secondResponse.Body.String())
	}
}

func TestFirstUserClaimsLegacySessions(t *testing.T) {
	server := newTestServer(t)
	id := createSession(t, server)
	request := httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader(`{"username":"owner","password":"password-123"}`))
	response := httptest.NewRecorder()
	server.handleRegister(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", response.Code, response.Body.String())
	}
	var user publicUser
	if err := json.Unmarshal(response.Body.Bytes(), &user); err != nil {
		t.Fatal(err)
	}
	managed, _ := server.getSession(id)
	if managed.meta.UserID != user.ID {
		t.Fatalf("legacy owner = %q, want %q", managed.meta.UserID, user.ID)
	}
}

func TestSessionOwnershipAndHistory(t *testing.T) {
	server := newTestServer(t)
	owner := requestPrincipal{UserID: "user-a", Username: "alice"}
	createRequest := requestWithPrincipal(httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader("{}")), owner)
	createResponse := httptest.NewRecorder()
	server.handleCreateSession(createResponse, createRequest)
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", createResponse.Code, createResponse.Body.String())
	}
	var created SessionInfoForTest
	if err := json.Unmarshal(createResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	materialize(t, server, created.ID)
	managed, _ := server.getSession(created.ID)
	managed.mu.Lock()
	managed.agentCtx = &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("继续这个历史问题")}},
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent("这是历史回答")}},
	}}
	msgs := managed.agentCtx.Messages
	managed.mu.Unlock()
	server.checkpoint(managed, msgs)

	otherRequest := requestWithPrincipal(httptest.NewRequest(http.MethodGet, "/api/sessions/"+created.ID, nil), requestPrincipal{UserID: "user-b"})
	otherRequest.SetPathValue("id", created.ID)
	otherResponse := httptest.NewRecorder()
	server.handleGetSession(otherResponse, otherRequest)
	if otherResponse.Code != http.StatusNotFound {
		t.Fatalf("other user get: %d", otherResponse.Code)
	}

	historyRequest := requestWithPrincipal(httptest.NewRequest(http.MethodGet, "/api/sessions/"+created.ID+"/messages", nil), owner)
	historyRequest.SetPathValue("id", created.ID)
	historyResponse := httptest.NewRecorder()
	server.handleSessionMessages(historyResponse, historyRequest)
	if historyResponse.Code != http.StatusOK || !strings.Contains(historyResponse.Body.String(), "历史回答") {
		t.Fatalf("history: %d %s", historyResponse.Code, historyResponse.Body.String())
	}

	listRequest := requestWithPrincipal(httptest.NewRequest(http.MethodGet, "/api/sessions", nil), owner)
	listResponse := httptest.NewRecorder()
	server.handleListSessions(listResponse, listRequest)
	if listResponse.Code != http.StatusOK || !strings.Contains(listResponse.Body.String(), "继续这个历史问题") {
		t.Fatalf("list: %d %s", listResponse.Code, listResponse.Body.String())
	}
}

func TestReapExpiredEmptySessionsOnly(t *testing.T) {
	server := newTestServer(t)
	server.config.emptySessionTTL = time.Minute
	emptyID := createSession(t, server)
	keptID := createSession(t, server)
	empty, _ := server.getSession(emptyID)
	kept, _ := server.getSession(keptID)
	empty.meta.CreatedAt = time.Now().Add(-2 * time.Minute)
	empty.meta.LastUsed = empty.meta.CreatedAt
	kept.meta.CreatedAt = time.Now().Add(-2 * time.Minute)
	kept.meta.LastUsed = kept.meta.CreatedAt
	if err := os.WriteFile(filepath.Join(kept.paths.Workspace, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	server.reapIdle()
	if _, ok := server.getSession(emptyID); ok {
		t.Fatal("expired empty session was not removed")
	}
	if _, ok := server.getSession(keptID); !ok {
		t.Fatal("session with workspace content was removed")
	}
}

type SessionInfoForTest struct {
	ID string `json:"id"`
}

func requestWithPrincipal(request *http.Request, principal requestPrincipal) *http.Request {
	return request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal))
}

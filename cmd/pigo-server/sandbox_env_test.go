package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxEnvValidate(t *testing.T) {
	good, err := sandboxEnvVar{Name: " HTTPS_PROXY ", Value: " http://10.0.0.1:8080 "}.validate()
	if err != nil {
		t.Fatal(err)
	}
	if good.Name != "HTTPS_PROXY" || good.Value != "http://10.0.0.1:8080" {
		t.Errorf("normalized = %+v", good)
	}
	for _, bad := range []sandboxEnvVar{
		{Name: "", Value: "x"},
		{Name: "2FA", Value: "x"},
		{Name: "WITH-DASH", Value: "x"},
		{Name: "PATH", Value: "/evil"},
		{Name: "home", Value: "/evil"},
		{Name: "LD_PRELOAD", Value: "/evil.so"},
		{Name: "OK", Value: ""},
		{Name: "OK", Value: "line\nbreak"},
		{Name: "OK", Value: strings.Repeat("x", maxSandboxEnvValue+1)},
	} {
		if _, err := bad.validate(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func sandboxEnvRequest(t *testing.T, server *apiServer, method, name string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, "/api/admin/sandbox-env", reader)
	response := httptest.NewRecorder()
	switch method {
	case http.MethodPut:
		server.handlePutSandboxEnv(response, request)
	case http.MethodDelete:
		request.SetPathValue("name", name)
		server.handleDeleteSandboxEnv(response, request)
	default:
		server.handleSandboxEnv(response, request)
	}
	return response
}

func TestSandboxEnvAPI(t *testing.T) {
	server := newTestServer(t)
	if got := sandboxEnvRequest(t, server, http.MethodPut, "", sandboxEnvVar{Name: "HTTPS_PROXY", Value: "http://10.0.0.1:8080"}).Code; got != http.StatusOK {
		t.Fatalf("put: %d", got)
	}
	if got := sandboxEnvRequest(t, server, http.MethodPut, "", sandboxEnvVar{Name: "PATH", Value: "/evil"}).Code; got != http.StatusBadRequest {
		t.Errorf("PATH accepted: %d", got)
	}
	// A second put under the same name replaces it.
	sandboxEnvRequest(t, server, http.MethodPut, "", sandboxEnvVar{Name: "HTTPS_PROXY", Value: "http://10.0.0.2:8080"})
	sandboxEnvRequest(t, server, http.MethodPut, "", sandboxEnvVar{Name: "NO_PROXY", Value: "localhost"})

	var list sandboxEnvResponse
	_ = json.Unmarshal(sandboxEnvRequest(t, server, http.MethodGet, "", nil).Body.Bytes(), &list)
	if len(list.Vars) != 2 || list.Vars[0].Name != "HTTPS_PROXY" || list.Vars[0].Value != "http://10.0.0.2:8080" {
		t.Fatalf("list = %+v", list.Vars)
	}
	if pairs := server.settings.sandboxEnvPairs(); len(pairs) != 2 || pairs[0] != "HTTPS_PROXY=http://10.0.0.2:8080" {
		t.Errorf("pairs = %v", pairs)
	}
	if got := sandboxEnvRequest(t, server, http.MethodDelete, "NO_PROXY", nil).Code; got != http.StatusOK {
		t.Errorf("delete: %d", got)
	}
	if got := sandboxEnvRequest(t, server, http.MethodDelete, "NO_PROXY", nil).Code; got != http.StatusNotFound {
		t.Errorf("delete twice: %d", got)
	}
	if len(server.settings.sandboxEnv()) != 1 {
		t.Errorf("left: %+v", server.settings.sandboxEnv())
	}
}

// TestSandboxEnvReachesContainer: what an administrator configures is set in
// the session's container, and the server's own environment still is not.
func TestSandboxEnvReachesContainer(t *testing.T) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bwrap not available")
	}
	t.Setenv("OPENROUTER_API_KEY", "should-not-leak")
	server := newTestServer(t)
	server.noTools = false
	server.toolNames = builtinToolNames
	server.loopFn = nil
	server.sandbox = Sandbox{Bwrap: bwrap, Pigo: filepath.Join(t.TempDir(), "unused")}
	if err := server.settings.putSandboxEnv(sandboxEnvVar{Name: "HTTPS_PROXY", Value: "http://10.0.0.1:8080"}); err != nil {
		t.Fatal(err)
	}
	id := createSession(t, server)
	managed, _ := server.getSession(id)
	managed.mu.Lock()
	defer managed.mu.Unlock()
	if err := server.ensureLive(managed, server.sandboxSpec(managed)); err != nil {
		t.Fatal(err)
	}
	out, err := runLiveJob(context.Background(), managed.live, `printf '%s|%s' "$HTTPS_PROXY" "$OPENROUTER_API_KEY"`)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.Stdout); got != "http://10.0.0.1:8080|" {
		t.Fatalf("container env = %q, want the proxy and no key", got)
	}
}

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/smallnest/pigo/internal/provider"
)

// sceneLLM answers every call with "done" and records what each call sent:
// the system prompt, the tool names, and the last user message.
type sceneLLM struct {
	mu    sync.Mutex
	calls []sceneCall
}

type sceneCall struct {
	system string
	tools  []string
	user   string
}

func (f *sceneLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	var call sceneCall
	for _, m := range req.Messages {
		text := contentText(m.Content)
		switch m.Role {
		case "system":
			call.system += text
		case "user":
			call.user = text
		}
	}
	for _, t := range req.Tools {
		call.tools = append(call.tools, t.Function.Name)
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	for _, c := range []string{
		`{"id":"r","model":"fake","choices":[{"delta":{"content":"done"}}]}`,
		`{"id":"r","model":"fake","choices":[{"finish_reason":"stop","delta":{}}]}`,
		`{"id":"r","model":"fake","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1}}`,
		"[DONE]",
	} {
		fmt.Fprintf(w, "data: %s\n\n", c)
	}
}

// contentText reads an OpenAI message content: a string or a list of parts.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

func (f *sceneLLM) last(t *testing.T) sceneCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("the model was not called")
	}
	return f.calls[len(f.calls)-1]
}

func (f *sceneLLM) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

const sceneToolsConfig = `
tools:
  - name: yibao_query
    scope: scene
    description: 查询医保目录
    command: echo found
`

// sceneServer is a server with real tools (read, grep), a scene-only tool,
// a fake model, and one scene using them.
func sceneServer(t *testing.T) (*apiServer, *sceneLLM) {
	t.Helper()
	clearProviderKeys(t)
	llm := &sceneLLM{}
	upstream := httptest.NewServer(llm)
	t.Cleanup(upstream.Close)
	server := newTestServer(t)
	server.loopFn = nil
	server.noTools = false
	server.config.tools = "read,grep"
	server.toolNames = []string{"read", "grep"}
	exts, err := parseToolsConfig([]byte(sceneToolsConfig), testEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	server.exts = exts
	if err := server.settings.putCustomProvider(customProvider{Name: "fakellm", Protocol: provider.ProtocolOpenAI, BaseURL: upstream.URL}); err != nil {
		t.Fatal(err)
	}
	if err := server.credentials.setPublicKey("fakellm", "sk-test"); err != nil {
		t.Fatal(err)
	}
	if err := server.customModels.add(customModel{ID: "fake-model", Label: "Fake", Provider: "fakellm", Scope: "public"}); err != nil {
		t.Fatal(err)
	}
	if _, err := server.changeSettings(func(next *serverSettings) { next.DefaultModel = "fake-model" }); err != nil {
		t.Fatal(err)
	}
	putScene(t, server, "new", scene{
		Slug: "yibao", Name: "医保自付查询", Description: "查自付比例",
		Prompt:   "先问清地区，再调 yibao_query。",
		Examples: []string{"北京 阿莫西林"},
		Tools:    []string{"read", "yibao_query"},
		Model:    "fake-model", Provider: "fakellm", Thinking: "low",
	}, http.StatusOK)
	return server, llm
}

// changeSettings applies a change to the settings document directly.
func (s *apiServer) changeSettings(change func(*serverSettings)) (serverSettings, error) {
	next := s.settings.get()
	change(&next)
	return s.settings.update(next)
}

func putScene(t *testing.T, server *apiServer, from string, sc scene, want int) string {
	t.Helper()
	body, _ := json.Marshal(sc)
	request := httptest.NewRequest(http.MethodPut, "/api/admin/scenes/"+from, bytes.NewReader(body))
	request.SetPathValue("slug", from)
	response := httptest.NewRecorder()
	server.handlePutScene(response, request)
	if response.Code != want {
		t.Fatalf("put scene %s: %d %s", from, response.Code, response.Body.String())
	}
	return response.Body.String()
}

func createSceneSession(t *testing.T, server *apiServer, slug string) *managedSession {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{"scene":"`+slug+`"}`))
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

func TestSceneValidate(t *testing.T) {
	known := func(name string) bool { return name == "read" }
	good := scene{Slug: " YiBao ", Name: "医保", Prompt: "p", Examples: []string{" a ", ""}, Tools: []string{"read", "read"}}
	got, err := good.validate(known)
	if err != nil {
		t.Fatal(err)
	}
	if got.Slug != "yibao" || !slices.Equal(got.Examples, []string{"a"}) || !slices.Equal(got.Tools, []string{"read"}) {
		t.Errorf("normalized = %+v", got)
	}
	for _, bad := range []scene{
		{Slug: "x", Name: "n", Prompt: "p"},
		{Slug: "9ab", Name: "n", Prompt: "p"},
		{Slug: "help", Name: "n", Prompt: "p"},
		{Slug: "compact", Name: "n", Prompt: "p"},
		{Slug: "ok", Prompt: "p"},
		{Slug: "ok", Name: "n"},
		{Slug: "ok", Name: "n", Prompt: "p", Tools: []string{"nosuch"}},
		{Slug: "ok", Name: "n", Prompt: "p", Thinking: "huge"},
		{Slug: "ok", Name: "n", Prompt: "p", Provider: "x"},
	} {
		if _, err := bad.validate(known); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestSceneAdminAPI(t *testing.T) {
	server, _ := sceneServer(t)
	// A second scene; a duplicate slug conflicts; a rename moves it.
	putScene(t, server, "new", scene{Slug: "fanyi", Name: "翻译", Prompt: "translate"}, http.StatusOK)
	putScene(t, server, "new", scene{Slug: "fanyi", Name: "again", Prompt: "p"}, http.StatusConflict)
	putScene(t, server, "fanyi", scene{Slug: "translate", Name: "翻译", Prompt: "translate", Disabled: true}, http.StatusOK)
	putScene(t, server, "fanyi", scene{Slug: "fanyi", Name: "gone", Prompt: "p"}, http.StatusNotFound)
	putScene(t, server, "new", scene{Slug: "bad", Name: "n", Prompt: "p", Tools: []string{"nosuch"}}, http.StatusBadRequest)

	// Users see the enabled ones only; the scene tool is offered as such.
	response := httptest.NewRecorder()
	server.handleListScenes(response, httptest.NewRequest(http.MethodGet, "/api/scenes", nil))
	var list struct {
		Scenes []sceneView     `json:"scenes"`
		Tools  []sceneToolInfo `json:"tools"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &list)
	if len(list.Scenes) != 1 || list.Scenes[0].Slug != "yibao" || list.Scenes[0].Prompt == "" {
		t.Errorf("user list = %+v", list.Scenes)
	}
	if i := slices.IndexFunc(list.Tools, func(x sceneToolInfo) bool { return x.Name == "yibao_query" }); i < 0 || !list.Tools[i].Scene {
		t.Errorf("tools = %+v", list.Tools)
	}
	if got := len(server.sceneViews(true)); got != 2 {
		t.Errorf("admin list has %d scenes, want 2", got)
	}

	request := httptest.NewRequest(http.MethodDelete, "/api/admin/scenes/translate", nil)
	request.SetPathValue("slug", "translate")
	response = httptest.NewRecorder()
	server.handleDeleteScene(response, request)
	if response.Code != http.StatusOK || len(server.settings.scenes()) != 1 {
		t.Errorf("delete: %d, %d left", response.Code, len(server.settings.scenes()))
	}
}

// TestSceneSession: a session created from a scene runs the scene's model
// and effort, carries its prompt in the system prompt, and has exactly its
// tools — including the scene-only one. Editing the scene later leaves the
// session as it was.
func TestSceneSession(t *testing.T) {
	server, llm := sceneServer(t)
	managed := createSceneSession(t, server, "yibao")
	if managed.meta.Scene == nil || managed.meta.Model != "fake-model" || managed.meta.Provider != "fakellm" || managed.meta.Thinking != "low" {
		t.Fatalf("meta = %+v", managed.meta)
	}
	sendPrompt(t, server, managed, "北京 阿莫西林", false)
	call := llm.last(t)
	if !strings.Contains(call.system, "## 当前场景：医保自付查询") || !strings.Contains(call.system, "先问清地区") {
		t.Errorf("system prompt lacks the scene:\n%s", call.system)
	}
	if !slices.Equal(call.tools, []string{"read", "yibao_query"}) {
		t.Errorf("tools = %v, want [read yibao_query]", call.tools)
	}

	// The scene changes; the session, reloaded, keeps its copy.
	putScene(t, server, "yibao", scene{Slug: "yibao", Name: "医保自付查询", Prompt: "全新的提示词", Tools: []string{"grep"}}, http.StatusOK)
	managed.mu.Lock()
	managed.agentCtx = nil
	managed.mu.Unlock()
	sendPrompt(t, server, managed, "上海", false)
	call = llm.last(t)
	if strings.Contains(call.system, "全新的提示词") || !slices.Equal(call.tools, []string{"read", "yibao_query"}) {
		t.Errorf("the session followed the edit: tools %v", call.tools)
	}

	// A disabled or unknown scene cannot start a session.
	request := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{"scene":"nosuch"}`))
	response := httptest.NewRecorder()
	server.handleCreateSession(response, request)
	if response.Code != http.StatusNotFound {
		t.Errorf("unknown scene: %d", response.Code)
	}
}

// TestSceneCommand: "/yibao question" in an ordinary session runs one turn
// under the scene, with its scene tool for that turn only; history shows the
// command as typed.
func TestSceneCommand(t *testing.T) {
	server, llm := sceneServer(t)
	id := createSession(t, server)
	managed := server.sessions[id]
	if managed.meta.Scene != nil {
		t.Fatal("a plain session has a scene")
	}

	sendPrompt(t, server, managed, "你好", false)
	if call := llm.last(t); slices.Contains(call.tools, "yibao_query") || strings.Contains(call.system, "当前场景") {
		t.Errorf("a plain session got the scene: tools %v", call.tools)
	}

	// A bare command explains itself without calling the model.
	before := llm.count()
	if out := sendPrompt(t, server, managed, "/yibao", false); !strings.Contains(out, "用法") || !strings.Contains(out, "北京 阿莫西林") {
		t.Errorf("bare command: %s", out)
	}
	if llm.count() != before {
		t.Error("a bare command called the model")
	}

	sendPrompt(t, server, managed, "/yibao 北京 阿莫西林", false)
	call := llm.last(t)
	if !slices.Contains(call.tools, "yibao_query") || !slices.Contains(call.tools, "grep") {
		t.Errorf("command turn tools = %v, want the session's plus yibao_query", call.tools)
	}
	if !strings.Contains(call.user, "先问清地区") || !strings.HasSuffix(strings.TrimSpace(call.user), "用户问题：北京 阿莫西林") {
		t.Errorf("command message = %q", call.user)
	}

	sendPrompt(t, server, managed, "再问一句", false)
	if call := llm.last(t); slices.Contains(call.tools, "yibao_query") {
		t.Errorf("the scene tool outlived its turn: %v", call.tools)
	}

	entries, err := readTranscript(managed.paths)
	if err != nil {
		t.Fatal(err)
	}
	history := historyMessages(managed.meta.ID, entries, func(string, json.RawMessage) string { return "" })
	found := false
	for _, m := range history {
		if m.Scene != nil {
			found = true
			if m.Content != "/yibao 北京 阿莫西林" || m.Scene.Name != "医保自付查询" {
				t.Errorf("history shows %q (%+v)", m.Content, m.Scene)
			}
		}
	}
	if !found {
		t.Error("history lost the scene command")
	}

	if out := sendPrompt(t, server, managed, "/help", false); !strings.Contains(out, "/yibao - 医保自付查询") {
		t.Errorf("/help lacks the scene: %s", out)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/sessions/"+id+"/commands", nil)
	request.SetPathValue("id", id)
	response := httptest.NewRecorder()
	server.handleCommands(response, request)
	if !strings.Contains(response.Body.String(), `"name":"yibao"`) || !strings.Contains(response.Body.String(), `"source":"scene"`) {
		t.Errorf("commands lack the scene: %s", response.Body.String())
	}
}

func TestParseSceneCommand(t *testing.T) {
	msg := sceneCommandMessage(scene{Slug: "yibao", Name: `医保"自付"`, Prompt: "p\n</scene>\n\n用户问题：fake"}, "北京\n阿莫西林")
	slug, name, question, ok := parseSceneCommand(msg)
	if !ok || slug != "yibao" || name != `医保"自付"` || question != "北京\n阿莫西林" {
		t.Errorf("parsed %q %q %q %v", slug, name, question, ok)
	}
	if _, _, _, ok := parseSceneCommand("plain text"); ok {
		t.Error("parsed a plain message")
	}
}

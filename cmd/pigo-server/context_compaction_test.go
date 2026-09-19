package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/provider"
)

func TestContextParams(t *testing.T) {
	server := newTestServer(t)

	// Built-in defaults until the deployment sets its own.
	p := server.contextParams("deepseek", "deepseek-v4-flash")
	if p.Window != 128000 || p.CompactPct != 80 || p.WindowSource != "default" || p.PctSource != "default" {
		t.Errorf("defaults = %+v", p)
	}
	set := p.settings()
	if !set.Enabled || set.ReserveTokens != 25600 || set.KeepRecentTokens != 20000 {
		t.Errorf("128k at 80%% = %+v, want reserve 25600, keep 20000", set)
	}
	if got := (contextParams{Window: 32000, CompactPct: 80}).settings().KeepRecentTokens; got != 8000 {
		t.Errorf("32k keeps %d, want a quarter: 8000", got)
	}

	if err := server.settings.setContextDefaults(200000, 85); err != nil {
		t.Fatal(err)
	}
	if err := server.settings.setContextDefaults(1000, 85); err == nil {
		t.Error("a 1k window was accepted")
	}
	if err := server.settings.setContextDefaults(200000, 99); err == nil {
		t.Error("99% was accepted")
	}

	// An override can set one field and keep the default for the other.
	if err := server.settings.putModelParam(modelParam{Provider: "codebuddy-proxy", Model: "global:hy3", ContextWindow: 64000}); err != nil {
		t.Fatal(err)
	}
	p = server.contextParams("codebuddy-proxy", "global:hy3")
	if p.Window != 64000 || p.WindowSource != "override" || p.CompactPct != 85 || p.PctSource != "default" {
		t.Errorf("window override = %+v", p)
	}

	// OpenRouter's catalog fills the window when nothing overrides it.
	server.openRouterWindows.store(map[string]int{"vendor/free:free": 262144})
	p = server.contextParams("openrouter", "vendor/free:free")
	if p.Window != 262144 || p.WindowSource != "openrouter" {
		t.Errorf("catalog window = %+v", p)
	}
	if err := server.settings.putModelParam(modelParam{Provider: "openrouter", Model: "vendor/free:free", ContextWindow: 100000}); err != nil {
		t.Fatal(err)
	}
	if p = server.contextParams("openrouter", "vendor/free:free"); p.Window != 100000 {
		t.Errorf("an override loses to the catalog: %+v", p)
	}

	for _, bad := range []modelParam{
		{Provider: "p", Model: "m"},
		{Provider: "p", Model: "m", ContextWindow: 100},
		{Provider: "p", Model: "m", CompactPct: 40},
		{Model: "m", CompactPct: 80},
	} {
		if _, err := bad.validate(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// compactionServer is a server whose sessions talk to a fake model, with a
// window small enough that every turn compacts.
func compactionServer(t *testing.T) (*apiServer, *fakeLLM, *managedSession) {
	t.Helper()
	clearProviderKeys(t)
	llm := &fakeLLM{}
	upstream := httptest.NewServer(llm)
	t.Cleanup(upstream.Close)
	server := newTestServer(t)
	server.loopFn = nil
	if err := server.settings.putCustomProvider(customProvider{Name: "fakellm", Protocol: provider.ProtocolOpenAI, BaseURL: upstream.URL}); err != nil {
		t.Fatal(err)
	}
	if err := server.credentials.setPublicKey("fakellm", "sk-test"); err != nil {
		t.Fatal(err)
	}
	if err := server.settings.putModelParam(modelParam{Provider: "fakellm", Model: "fake-model", ContextWindow: 200, CompactPct: 95}); err != nil {
		t.Fatal(err)
	}
	id := createSession(t, server)
	managed := server.sessions[id]
	managed.meta.Model, managed.meta.Provider = "fake-model", "fakellm"
	managed.mu.Lock()
	if err := server.ensureHostLoop(managed); err != nil {
		t.Fatal(err)
	}
	// An earlier exchange long enough to be worth summarizing.
	long := strings.Repeat("earlier context ", 400)
	managed.agentCtx.Messages = agentcore.MessageList{
		textMessage(agentcore.RoleUser, long),
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent(long)}, StopReason: agentcore.StopReasonEndTurn},
	}
	server.checkpointLocked(managed, managed.agentCtx.Messages)
	managed.mu.Unlock()
	return server, llm, managed
}

func sendPrompt(t *testing.T, server *apiServer, managed *managedSession, prompt string, stream bool) string {
	t.Helper()
	body, _ := json.Marshal(messageRequest{Prompt: prompt})
	target := "/api/sessions/" + managed.meta.ID + "/messages"
	if stream {
		target += "?stream=true"
	}
	request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	request.SetPathValue("id", managed.meta.ID)
	response := httptest.NewRecorder()
	server.handleMessage(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
	return response.Body.String()
}

// TestAutoCompactionKeepsHistory runs a turn that compacts: the events reach
// the client, the transcript keeps every message and gains the summary, the
// context reloads as summary + tail, and history shows the compaction.
func TestAutoCompactionKeepsHistory(t *testing.T) {
	server, llm, managed := compactionServer(t)
	out := sendPrompt(t, server, managed, "hi", true)
	if !strings.Contains(strings.Join(llm.calls(), ","), "summary") {
		t.Fatalf("no summary call: %v", llm.calls())
	}
	if !strings.Contains(out, `"type":"compaction","phase":"start"`) || !strings.Contains(out, `"type":"compaction","phase":"end"`) {
		t.Errorf("stream lacks the compaction events:\n%s", out)
	}

	entries, err := readTranscript(managed.paths)
	if err != nil {
		t.Fatal(err)
	}
	var compactions, earlier int
	for _, e := range entries {
		switch m := e.Message.(type) {
		case agentcore.CompactionMessage:
			compactions++
		case agentcore.UserMessage:
			if strings.HasPrefix(agentcore.ContentToText(m.Content), "earlier context") {
				earlier++
			}
		}
	}
	if compactions == 0 || earlier != 1 {
		t.Fatalf("transcript: %d compactions, earlier message kept %d times (want ≥1 and 1)", compactions, earlier)
	}

	// Reloaded, the context starts with the summary.
	fresh := &managedSession{paths: managed.paths, meta: managed.meta}
	if msgs := server.loadTranscript(fresh); len(msgs) == 0 || msgs[0].Role() != agentcore.RoleCompaction {
		t.Fatalf("reloaded context starts with %v", msgs)
	}

	history := historyMessages(managed.meta.ID, entries, func(string, json.RawMessage) string { return "" })
	found := false
	for _, m := range history {
		if m.Role == agentcore.RoleCompaction && m.Compaction != nil && m.Content != "" {
			found = true
		}
	}
	if !found || history[0].Role != agentcore.RoleUser {
		t.Errorf("history lacks the compaction or the earlier messages: %+v", history)
	}
}

func TestCompactCommand(t *testing.T) {
	// A short session has nothing to compact.
	server, _, managed := compactionServer(t)
	managed.mu.Lock()
	managed.agentCtx.Messages = managed.agentCtx.Messages[:0]
	managed.mu.Unlock()
	if out := sendPrompt(t, server, managed, "/compact", false); !strings.Contains(out, "没有可以压缩") {
		t.Errorf("short session: %s", out)
	}

	// A long one is summarized now, whatever the threshold.
	server, llm, managed := compactionServer(t)
	if err := server.settings.putModelParam(modelParam{Provider: "fakellm", Model: "fake-model", ContextWindow: 1_000_000, CompactPct: 95}); err != nil {
		t.Fatal(err)
	}
	managed.mu.Lock()
	for i := 0; i < 20; i++ {
		managed.agentCtx.Messages = append(managed.agentCtx.Messages,
			textMessage(agentcore.RoleUser, strings.Repeat("more ", 2000)),
			agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent("ok")}, StopReason: agentcore.StopReasonEndTurn})
	}
	server.checkpointLocked(managed, managed.agentCtx.Messages)
	managed.mu.Unlock()
	out := sendPrompt(t, server, managed, "/compact", false)
	if !strings.Contains(out, "已压缩上下文") || strings.Join(llm.calls(), ",") != "summary" {
		t.Fatalf("reply = %s, calls = %v", out, llm.calls())
	}
	managed.mu.Lock()
	first := managed.agentCtx.Messages[0].Role()
	managed.mu.Unlock()
	if first != agentcore.RoleCompaction {
		t.Errorf("context starts with %s after /compact", first)
	}
	entries, _ := readTranscript(managed.paths)
	if n := len(entries); n != 43 || entries[n-1].Message.Role() != agentcore.RoleCompaction {
		t.Errorf("transcript has %d entries ending in %s, want the 42 messages then the summary", n, entries[n-1].Message.Role())
	}
}

// TestSkippedCompactionLeavesNoTrace: over the threshold with nothing old
// enough to summarize, the loop starts a compaction and returns without an
// end. The server closes it as skipped, and the activity log drops it.
func TestSkippedCompactionLeavesNoTrace(t *testing.T) {
	server, llm, managed := compactionServer(t)
	managed.mu.Lock()
	managed.agentCtx.Messages = managed.agentCtx.Messages[:0]
	managed.mu.Unlock()
	out := sendPrompt(t, server, managed, "hi", true)
	if strings.Contains(strings.Join(llm.calls(), ","), "summary") {
		t.Fatalf("summarized with nothing to summarize: %v", llm.calls())
	}
	starts := strings.Count(out, `"type":"compaction","phase":"start"`)
	ends := strings.Count(out, `"type":"compaction","phase":"end"`)
	if starts == 0 || starts != ends {
		t.Fatalf("compaction starts %d, ends %d:\n%s", starts, ends, out)
	}
	managed.mu.Lock()
	run := managed.turn
	managed.mu.Unlock()
	snapshot, _, _ := run.subscribe()
	for _, item := range snapshot.Activity {
		if item.Kind == "compaction" {
			t.Errorf("a skipped compaction stayed in the log: %+v", item)
		}
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/provider"
)

// taskLLM plays both sides of a sub-agent run: as the parent it dispatches one
// task and then answers; as the sub-agent (its tool set has no task) it calls
// one tool and then reports.
type taskLLM struct {
	mu sync.Mutex
	// calls records one line per request: "parent:<kind>" or "child:<kind>".
	calls []string
	// childArgs is what the parent asks the sub-agent to do.
	childArgs string
	// childTools is the sub-agent's tool set, as the model saw it.
	childTools []string
	// childSystem is the sub-agent's system prompt, as the model saw it.
	childSystem string
	// childReport is what the sub-agent answers with.
	childReport string
	// childTool is the tool the sub-agent calls, when it has one.
	childTool string
	// dispatchArgs overrides what the parent asks for, when a test sets it.
	dispatchArgs string
}

func (f *taskLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	var tools []string
	for _, t := range req.Tools {
		tools = append(tools, t.Function.Name)
	}
	parent := slices.Contains(tools, "task")
	last := ""
	if len(req.Messages) > 0 {
		last = req.Messages[len(req.Messages)-1].Role
	}

	w.Header().Set("Content-Type", "text/event-stream")
	send := func(o string) { fmt.Fprintf(w, "data: %s\n\n", o) }
	defer func() {
		send(`{"id":"r","model":"fake","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":10}}`)
		send("[DONE]")
	}()

	f.mu.Lock()
	if !parent {
		f.childTools = tools
		f.childSystem = ""
		for _, m := range req.Messages {
			if m.Role == "system" {
				f.childSystem += contentText(m.Content)
			}
		}
	}
	childTool, report := f.childTool, f.childReport
	f.mu.Unlock()

	switch {
	case parent && last == "user":
		f.record("parent:dispatch")
		args := `{"prompt":"look up the version","description":"查版本","subagent_type":"explore"}`
		f.mu.Lock()
		if f.dispatchArgs != "" {
			args = f.dispatchArgs
		}
		f.childArgs = args
		f.mu.Unlock()
		send(`{"id":"r","model":"fake","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_task","type":"function","function":{"name":"task","arguments":` + quote(args) + `}}]}}]}`)
		send(`{"id":"r","model":"fake","choices":[{"finish_reason":"tool_calls","delta":{}}]}`)
	case parent:
		f.record("parent:answer")
		send(`{"id":"r","model":"fake","choices":[{"delta":{"content":"主线收到子代理的报告。"}}]}`)
		send(`{"id":"r","model":"fake","choices":[{"finish_reason":"stop","delta":{}}]}`)
	case last == "user" && childTool != "" && slices.Contains(tools, childTool):
		f.record("child:tool")
		send(`{"id":"r","model":"fake","choices":[{"delta":{"content":"先看看文件。"}}]}`)
		send(`{"id":"r","model":"fake","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_child","type":"function","function":{"name":"` + childTool + `","arguments":"{\"path\":\"note.txt\"}"}}]}}]}`)
		send(`{"id":"r","model":"fake","choices":[{"finish_reason":"tool_calls","delta":{}}]}`)
	default:
		f.record("child:report")
		send(`{"id":"r","model":"fake","choices":[{"delta":{"content":` + quote(report) + `}}]}`)
		send(`{"id":"r","model":"fake","choices":[{"finish_reason":"stop","delta":{}}]}`)
	}
}

func (f *taskLLM) record(what string) {
	f.mu.Lock()
	f.calls = append(f.calls, what)
	f.mu.Unlock()
}

func (f *taskLLM) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func quote(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// taskServer is a server whose sessions run real tools against a fake model
// that dispatches one sub-agent.
func taskServer(t *testing.T) (*apiServer, *taskLLM, *managedSession) {
	t.Helper()
	clearProviderKeys(t)
	llm := &taskLLM{childReport: "版本是 1.2.3。", childTool: "read"}
	upstream := httptest.NewServer(llm)
	t.Cleanup(upstream.Close)
	server := newTestServer(t)
	server.loopFn = nil
	server.noTools = false
	server.config.tools = "all"
	server.toolNames = append([]string(nil), hostToolNames...)
	if err := server.settings.putCustomProvider(customProvider{Name: "fakellm", Protocol: provider.ProtocolOpenAI, BaseURL: upstream.URL}); err != nil {
		t.Fatal(err)
	}
	if err := server.credentials.setPublicKey("fakellm", "sk-test"); err != nil {
		t.Fatal(err)
	}
	if err := server.customModels.add(customModel{ID: "fake-model", Label: "Fake", Provider: "fakellm", Scope: "public"}); err != nil {
		t.Fatal(err)
	}
	ledger, err := newLedgerStore(server.db)
	if err != nil {
		t.Fatal(err)
	}
	server.ledger = ledger
	server.meter = &meter{ledger: ledger, settings: server.settings}
	id := createSession(t, server)
	managed := server.sessions[id]
	managed.meta.Model, managed.meta.Provider = "fake-model", "fakellm"
	return server, llm, managed
}

func mustType(t *testing.T, name string) (subagentType, error) {
	t.Helper()
	return findSubagentType(name)
}

func TestSubagentToolSet(t *testing.T) {
	server, _, managed := taskServer(t)

	general, err := mustType(t, defaultSubagentType)
	explore, err2 := mustType(t, "explore")
	if err2 != nil {
		t.Fatal(err2)
	}
	full, err := server.subagentTools(managed, general, 20)
	if err != nil {
		t.Fatal(err)
	}
	names := func(tools []agentcore.AgentTool) []string {
		var out []string
		for _, tool := range tools {
			out = append(out, tool.Name())
		}
		return out
	}
	if got := names(full); slices.Contains(got, "task") || !slices.Contains(got, "bash") {
		t.Errorf("general-purpose = %v, want the session's tools without task", got)
	}
	readonly, err := server.subagentTools(managed, explore, 20)
	if err != nil {
		t.Fatal(err)
	}
	got := names(readonly)
	// explore keeps everything but the editing tools: looking around a
	// workspace takes a shell, as Claude Code's Explore agent has one.
	if slices.Contains(got, "write") || slices.Contains(got, "edit") || slices.Contains(got, "task") {
		t.Errorf("explore = %v, want no editing tools", got)
	}
	for _, want := range []string{"read", "grep", "find", "bash", "webfetch", "websearch"} {
		if !slices.Contains(got, want) {
			t.Errorf("explore lacks %s: %v", want, got)
		}
	}

	// An unknown type is refused by name, with the known ones listed.
	if _, err := findSubagentType("root"); err == nil || !strings.Contains(err.Error(), "explore") {
		t.Errorf("unknown type: %v", err)
	}
	// No type means the general-purpose one, which gives up nothing.
	if kind, err := findSubagentType(""); err != nil || len(kind.without) > 0 {
		t.Errorf("default type = %+v (%v)", kind, err)
	}

	// The parent has task; a scene session and a disabled deployment do not.
	parent, err := server.hostTools(managed)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names(parent), "task") {
		t.Error("the session has no task tool")
	}
	managed.meta.Scene = &sceneSnapshot{Slug: "x", Name: "X", Prompt: "p"}
	parent, _ = server.hostTools(managed)
	if slices.Contains(names(parent), "task") {
		t.Error("a scene session got the task tool")
	}
	managed.meta.Scene = nil
	if err := server.settings.setSubagent(subagentSettings{Disabled: true}); err != nil {
		t.Fatal(err)
	}
	parent, _ = server.hostTools(managed)
	if slices.Contains(names(parent), "task") {
		t.Error("task survived the deployment switch")
	}
}

func TestSubagentStepLimit(t *testing.T) {
	server, _, managed := taskServer(t)
	explore, err := mustType(t, "explore")
	if err != nil {
		t.Fatal(err)
	}
	tools, err := server.subagentTools(managed, explore, 1)
	if err != nil {
		t.Fatal(err)
	}
	var read agentcore.AgentTool
	for _, tool := range tools {
		if tool.Name() == "read" {
			read = tool
		}
	}
	args := json.RawMessage(`{"path":"nope.txt"}`)
	// The first call reaches the tool itself; the second is refused by the
	// limit, with an answer that asks the sub-agent for a conclusion.
	first, _ := read.Execute(context.Background(), "1", args, nil)
	if strings.Contains(agentcore.ContentToText(first.Content), "步数上限") {
		t.Fatal("the first call was already over the limit")
	}
	out, err := read.Execute(context.Background(), "2", args, nil)
	if err != nil {
		t.Fatalf("over the limit should refuse, not fail: %v", err)
	}
	if !strings.Contains(agentcore.ContentToText(out.Content), "步数上限") {
		t.Errorf("limit result = %q", agentcore.ContentToText(out.Content))
	}
}

func TestSubagentModelWhitelist(t *testing.T) {
	server, _, managed := taskServer(t)
	tool := &taskTool{server: server, session: managed}
	call := func(model string) (string, error) {
		args, _ := json.Marshal(taskArgs{Prompt: "hi", Model: model})
		out, err := tool.Execute(context.Background(), "c1", args, nil)
		return agentcore.ContentToText(out.Content), err
	}
	_, err := call("other-model")
	if err == nil || !strings.Contains(err.Error(), "不在允许的清单里") {
		t.Fatalf("an unlisted model was accepted: %v", err)
	}
	if err := server.settings.setSubagent(subagentSettings{Models: []string{"fake-model"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := call("still-not-listed"); err == nil {
		t.Error("an unlisted model was accepted with a list configured")
	}
}

// TestSubagentRun is the whole path: the parent dispatches, the sub-agent runs
// its own loop with its own tools, its steps land under the task call in the
// activity log, its calls are metered as "subagent" in the same turn, and its
// report comes back to the parent.
func TestSubagentRun(t *testing.T) {
	server, llm, managed := taskServer(t)
	requireSandbox(t, server)
	out := sendPrompt(t, server, managed, "去查一下版本", true)
	if !strings.Contains(out, "主线收到子代理的报告") {
		t.Fatalf("stream lacks the answer:\n%s", out)
	}
	if got := llm.seen(); !slices.Contains(got, "parent:dispatch") || !slices.Contains(got, "child:tool") || !slices.Contains(got, "child:report") {
		t.Fatalf("calls = %v", got)
	}
	llm.mu.Lock()
	childTools := append([]string(nil), llm.childTools...)
	llm.mu.Unlock()
	if slices.Contains(childTools, "task") {
		t.Errorf("the sub-agent could dispatch further sub-agents: %v", childTools)
	}
	// explore keeps the shell (that is how it looks around) but not editing.
	if !slices.Contains(childTools, "bash") || slices.Contains(childTools, "edit") {
		t.Errorf("explore tools = %v", childTools)
	}

	managed.mu.Lock()
	run := managed.turn
	managed.mu.Unlock()
	snapshot, _, _ := run.subscribe()
	var task *activityItem
	for i := range snapshot.Activity {
		if snapshot.Activity[i].Tool == "task" {
			task = &snapshot.Activity[i]
		}
	}
	if task == nil {
		t.Fatalf("no task item in the log: %+v", snapshot.Activity)
	}
	// The line names the job and, when it is not the default, the agent type.
	if task.Detail != "查版本 · explore" || task.Status != "ok" {
		t.Errorf("task item = %+v", *task)
	}
	var childCalls, childText int
	for _, child := range task.Children {
		switch child.Kind {
		case "tool":
			childCalls++
			if child.Tool != "read" || child.Status == "running" {
				t.Errorf("child call = %+v", child)
			}
		case "text":
			childText++
		}
	}
	if childCalls == 0 || childText == 0 {
		t.Errorf("the sub-agent's steps are not under the task call: %+v", task.Children)
	}

	// Its calls are in this turn's ledger, labelled and billed like the rest.
	entries, err := server.ledger.scan(ledgerQuery{SessionID: managed.meta.ID})
	if err != nil {
		t.Fatal(err)
	}
	var subagent, chat int
	for _, e := range entries {
		switch e.Kind {
		case "subagent":
			subagent++
			if e.TurnID == "" || e.SessionID != managed.meta.ID {
				t.Errorf("sub-agent call outside the turn: %+v", e)
			}
		case "chat":
			chat++
		}
	}
	if subagent == 0 || chat == 0 {
		t.Errorf("ledger kinds: %d subagent, %d chat", subagent, chat)
	}
}

// TestSubagentGate: the session's gate admits only as many as configured, and
// releases when a sub-agent ends.
func TestSubagentGate(t *testing.T) {
	gate := newSubagentGate()
	ctx := context.Background()
	if err := gate.acquire(ctx, 1); err != nil {
		t.Fatal(err)
	}
	waiting := make(chan error, 1)
	go func() { waiting <- gate.acquire(ctx, 1) }()
	select {
	case err := <-waiting:
		t.Fatalf("the second acquire did not wait: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	gate.release()
	select {
	case err := <-waiting:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("releasing did not admit the waiter")
	}
	gate.release()

	// A cancelled turn does not leave one parked.
	cancelled, cancel := context.WithCancel(context.Background())
	if err := gate.acquire(cancelled, 1); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- gate.acquire(cancelled, 1) }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("acquire returned a slot after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not wake the waiter")
	}
	gate.release()
}

func TestSubagentSettingsAPI(t *testing.T) {
	server := newTestServer(t)
	put := func(body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		request := httptest.NewRequest(http.MethodPut, "/api/admin/subagent", bytes.NewReader(raw))
		response := httptest.NewRecorder()
		server.handlePutSubagent(response, request)
		return response
	}
	if code := put(subagentSettings{MaxConcurrent: 2, MaxSteps: 5, TimeoutSeconds: 60, Models: []string{"a", " a ", ""}}).Code; code != http.StatusOK {
		t.Fatalf("put: %d", code)
	}
	got := server.settings.subagent()
	if got.concurrent() != 2 || got.steps() != 5 || got.timeout() != time.Minute || !slices.Equal(got.Models, []string{"a"}) {
		t.Errorf("stored = %+v", got)
	}
	if code := put(subagentSettings{MaxConcurrent: 99}).Code; code != http.StatusBadRequest {
		t.Errorf("an absurd concurrency was accepted: %d", code)
	}
	// Defaults answer for a deployment that never set anything.
	fresh := newTestServer(t)
	response := httptest.NewRecorder()
	fresh.handleSubagent(response, httptest.NewRequest(http.MethodGet, "/api/admin/subagent", nil))
	var view subagentResponse
	_ = json.Unmarshal(response.Body.Bytes(), &view)
	if view.Defaults.MaxSteps != defaultSubagentSteps || view.Settings.Disabled {
		t.Errorf("fresh view = %+v", view)
	}
}

// TestClipReportCutsOnRunes: a report is usually Chinese, and the limit must
// not split a character — a byte cut would put a broken rune into the parent's
// context and the transcript.
func TestClipReportCutsOnRunes(t *testing.T) {
	long := strings.Repeat("子", subagentReportLimit+10)
	got := clipReport(long)
	if !utf8.ValidString(got) {
		t.Fatal("clipReport produced invalid UTF-8")
	}
	if !strings.HasSuffix(got, "（已截断，子代理的原文更长）") {
		t.Errorf("no truncation marker: %q", got[len(got)-40:])
	}
	if n := utf8.RuneCountInString(strings.Split(got, "\n\n（已截断")[0]); n != subagentReportLimit {
		t.Errorf("kept %d runes, want %d", n, subagentReportLimit)
	}
	short := "版本是 1.2.3。"
	if clipReport(short) != short {
		t.Error("a short report was touched")
	}
}

// TestSubagentOwnModelAllowed: naming the conversation's own model is what a
// call gets by leaving the field out, so a whitelist must not refuse it.
func TestSubagentOwnModelAllowed(t *testing.T) {
	server, llm, managed := taskServer(t)
	requireSandbox(t, server)
	if err := server.settings.setSubagent(subagentSettings{Models: []string{"other-model"}}); err != nil {
		t.Fatal(err)
	}
	llm.mu.Lock()
	llm.dispatchArgs = `{"prompt":"look up the version","description":"查版本","subagent_type":"explore","model":"fake-model"}`
	llm.mu.Unlock()
	out := sendPrompt(t, server, managed, "去查一下版本", true)
	if strings.Contains(out, "不在允许的清单里") {
		t.Fatalf("the session's own model was refused:\n%s", out)
	}
	if got := llm.seen(); !slices.Contains(got, "child:report") {
		t.Fatalf("the sub-agent did not run: %v", got)
	}
}

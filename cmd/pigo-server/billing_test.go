package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/compaction"
	"github.com/smallnest/pigo/internal/provider"
)

func ptr(v float64) *float64 { return &v }

// TestRateDeepSeek prices the captured DeepSeek warm call (205 uncached + 768
// cached in, 1 out) at DeepSeek's list prices: 2 / 0.5 / 8 yuan per million.
func TestRateDeepSeek(t *testing.T) {
	price := modelPrice{Input: 2, CacheRead: ptr(0.5), Output: 8}.effective()
	got := rate(agentcore.Usage{InputTokens: 205, CacheReadTokens: 768, OutputTokens: 1}, price)
	// 205×2 + 768×0.5 + 1×8 = 802 yuan-tokens per million = 802,000 nano-yuan.
	if got != 802000 {
		t.Errorf("cost = %d nano-yuan, want 802000", got)
	}
}

// TestRateFallbacksAndReasoning checks the unset cache prices fall back to the
// input price, and that reasoning (inside output) is not charged twice.
func TestRateFallbacksAndReasoning(t *testing.T) {
	price := modelPrice{Input: 3, Output: 15}.effective()
	if price.CacheRead != 3 || price.CacheWrite != 3 {
		t.Fatalf("fallback = %+v", price)
	}
	u := agentcore.Usage{InputTokens: 1000, CacheReadTokens: 1000, CacheWriteTokens: 1000, OutputTokens: 1000, ReasoningTokens: 900}
	if got := rate(u, price); got != (3+3+3+15)*1000*1000 {
		t.Errorf("cost = %d", got)
	}
}

// TestCostOfRoundsAndDoesNotOverflow covers rounding of sub-nano amounts and
// products beyond the int64 range of a naive tokens × nano-price.
func TestCostOfRoundsAndDoesNotOverflow(t *testing.T) {
	if got := costOf(1, 0.0004); got != 0 {
		t.Errorf("0.4 nano rounds to %d, want 0", got)
	}
	if got := costOf(1, 0.0005); got != 1 {
		t.Errorf("0.5 nano rounds to %d, want 1", got)
	}
	// 2e9 tokens at 100000 yuan/M = 2e8 yuan = 2e17 nano; the intermediate
	// product is 2e9 × 1e14 = 2e23, far past int64.
	if got := costOf(2_000_000_000, 100000); got != 200_000_000*1_000_000_000 {
		t.Errorf("large cost = %d", got)
	}
}

func TestPriceValidate(t *testing.T) {
	if _, err := (modelPrice{Provider: "x", Model: "m", Input: -1}).validate(); err == nil {
		t.Error("negative price accepted")
	}
	if _, err := (modelPrice{Provider: "x", Model: "m", Input: 1, CacheRead: ptr(200000)}).validate(); err == nil {
		t.Error("absurd cache price accepted")
	}
	got, err := (modelPrice{Provider: " DeepSeek ", Model: " deepseek-chat ", Input: 2, Output: 8}).validate()
	if err != nil || got.Provider != "deepseek" || got.Model != "deepseek-chat" {
		t.Errorf("normalized = %+v, %v", got, err)
	}
}

// TestPriceForPrefersResponseModel: an alias answer is priced as the model that
// answered, and the requested model is the fallback.
func TestPriceForPrefersResponseModel(t *testing.T) {
	forEachDB(t, testPriceFor)
}

func testPriceFor(t *testing.T, target dbTarget) {
	db := mustOpen(t, target)
	cfg := serverConfig{model: "m", thinking: "medium"}
	store, err := newSettingsStore(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []modelPrice{
		{Provider: "deepseek", Model: "deepseek-chat", Input: 2, Output: 8},
		{Provider: "deepseek", Model: "deepseek-flash", Input: 1, Output: 4},
	} {
		if err := store.putPrice(row); err != nil {
			t.Fatal(err)
		}
	}
	if p, ok := store.priceFor("deepseek", "deepseek-flash", "deepseek-chat"); !ok || p.Input != 1 {
		t.Errorf("answered model not preferred: %+v %v", p, ok)
	}
	if p, ok := store.priceFor("deepseek", "unknown-alias", "deepseek-chat"); !ok || p.Input != 2 {
		t.Errorf("requested model not the fallback: %+v %v", p, ok)
	}
	if _, ok := store.priceFor("other", "deepseek-chat", "deepseek-chat"); ok {
		t.Error("price leaked across providers")
	}
	// Replacing keeps one row; removing drops it.
	if err := store.putPrice(modelPrice{Provider: "deepseek", Model: "deepseek-chat", Input: 5, Output: 8}); err != nil {
		t.Fatal(err)
	}
	if n := len(store.prices()); n != 2 {
		t.Errorf("rows = %d after replace, want 2", n)
	}
	if removed, err := store.removePrice("deepseek", "deepseek-chat"); !removed || err != nil {
		t.Errorf("remove = %v %v", removed, err)
	}
	// Prices survive a restart.
	db.Close()
	reloaded, err := newSettingsStore(mustOpen(t, target), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if rows := reloaded.prices(); len(rows) != 1 || rows[0].Model != "deepseek-flash" {
		t.Errorf("reloaded = %+v", rows)
	}
}

func TestLedgerAppendAndScan(t *testing.T) {
	forEachDB(t, testLedger)
}

func testLedger(t *testing.T, target dbTarget) {
	ledger, err := newLedgerStore(mustOpen(t, target))
	if err != nil {
		t.Fatal(err)
	}
	aug := time.Date(2026, 8, 31, 23, 0, 0, 0, time.UTC)
	sep := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	for _, e := range []ledgerEntry{
		{ID: "a", At: aug, UserID: "u1", SessionID: "s1", Provider: "deepseek", Model: "deepseek-chat", Cost: 10},
		{ID: "b", At: sep, UserID: "u1", SessionID: "s2", Provider: "deepseek", Model: "deepseek-chat", ResponseModel: "deepseek-flash", Cost: 20},
		{ID: "c", At: sep.Add(time.Hour), UserID: "u2", SessionID: "s3", Provider: "anthropic", Model: "claude", Cost: 30},
	} {
		if err := ledger.append(e); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(q ledgerQuery) string {
		entries, err := ledger.scan(q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range entries {
			out = append(out, e.ID)
		}
		return strings.Join(out, ",")
	}
	cases := []struct {
		name string
		q    ledgerQuery
		want string
	}{
		{"all", ledgerQuery{}, "a,b,c"},
		{"september", ledgerQuery{From: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}, "b,c"},
		{"before september", ledgerQuery{To: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}, "a"},
		{"user", ledgerQuery{UserID: "u1"}, "a,b"},
		{"session", ledgerQuery{SessionID: "s2"}, "b"},
		{"answered model", ledgerQuery{Model: "deepseek-flash"}, "b"},
		{"provider", ledgerQuery{Provider: "anthropic"}, "c"},
	}
	for _, tc := range cases {
		if got := ids(tc.q); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// --- the meter ---------------------------------------------------------------

// scriptedStream returns a StreamFn that plays the given events for each call.
func scriptedStream(events ...provider.AssistantMessageEvent) provider.StreamFn {
	return func(ctx context.Context, _ string, _ provider.LlmContext, _ provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		s := provider.NewAssistantMessageEventStream(0)
		go func() {
			defer s.Close()
			for _, ev := range events {
				if s.Emit(ctx, ev) != nil {
					return
				}
			}
		}()
		return s, nil
	}
}

func assistant(text string, usage *agentcore.Usage) agentcore.AssistantMessage {
	return agentcore.AssistantMessage{
		RoleField:     agentcore.RoleAssistant,
		Content:       agentcore.ContentList{agentcore.NewTextContent(text)},
		ResponseID:    "resp-1",
		ResponseModel: "deepseek-flash",
		Usage:         usage,
	}
}

type meterFixture struct {
	meter  *meter
	ledger *ledgerStore
	mu     sync.Mutex
	events []streamEvent
}

func newMeterFixture(t *testing.T) *meterFixture {
	t.Helper()
	db := openTestDB(t)
	ledger, err := newLedgerStore(db)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := newSettingsStore(db, serverConfig{model: "m", thinking: "medium"})
	if err != nil {
		t.Fatal(err)
	}
	if err := settings.putPrice(modelPrice{Provider: "deepseek", Model: "deepseek-flash", Input: 2, CacheRead: ptr(0.5), Output: 8}); err != nil {
		t.Fatal(err)
	}
	return &meterFixture{meter: &meter{ledger: ledger, settings: settings}, ledger: ledger}
}

func (f *meterFixture) turn(keySource string) *turnInfo {
	return &turnInfo{userID: "u1", username: "alice", sessionID: "s1", turnID: "t1", keySource: keySource,
		emit: func(ev streamEvent) {
			f.mu.Lock()
			f.events = append(f.events, ev)
			f.mu.Unlock()
		}}
}

// drain consumes a stream the way the agent loop does.
func drain(t *testing.T, ctx context.Context, s *provider.AssistantMessageEventStream) (agentcore.AssistantMessage, error) {
	t.Helper()
	for range s.Events() {
	}
	return s.Result(ctx)
}

func (f *meterFixture) entries(t *testing.T) []ledgerEntry {
	t.Helper()
	entries, err := f.ledger.scan(ledgerQuery{})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// TestMeterRecordsCompletedCall: the stream passes through unchanged, and the
// call is in the ledger — priced as the model that answered — before the
// consumer sees it end.
func TestMeterRecordsCompletedCall(t *testing.T) {
	f := newMeterFixture(t)
	usage := &agentcore.Usage{InputTokens: 205, CacheReadTokens: 768, OutputTokens: 1}
	final := assistant("ok", usage)
	stream := f.meter.wrap(scriptedStream(
		provider.StreamStartEvent{Partial: assistant("", nil)},
		provider.StreamTextEvent{Partial: assistant("ok", nil)},
		provider.StreamDoneEvent{Message: final},
	), "deepseek", "chat")

	turn := f.turn("user")
	ctx := withTurn(context.Background(), turn)
	s, err := stream(ctx, "deepseek-chat", provider.LlmContext{}, provider.StreamConfig{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := drain(t, ctx, s)
	if err != nil || agentcore.ContentToText(got.Content) != "ok" {
		t.Fatalf("result = %+v, %v", got, err)
	}
	// Recorded before the stream closed: no waiting needed.
	entries := f.entries(t)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Status != usageOK || e.Kind != "chat" || e.Model != "deepseek-chat" || e.ResponseModel != "deepseek-flash" {
		t.Errorf("entry = %+v", e)
	}
	if !e.Priced || e.Cost != 802000 || e.Price == nil || e.Price.CacheRead != 0.5 {
		t.Errorf("pricing = priced %v cost %d price %+v", e.Priced, e.Cost, e.Price)
	}
	if e.BilledTo != billedSelf || e.Username != "alice" || e.ResponseID != "resp-1" {
		t.Errorf("attribution = %+v", e)
	}
	if len(f.events) != 1 || f.events[0].Type != "usage" || f.events[0].Usage.Cost != 0.000802 {
		t.Errorf("usage events = %+v", f.events)
	}
}

// TestMeterStatuses covers each way a call can end.
func TestMeterStatuses(t *testing.T) {
	usage := &agentcore.Usage{InputTokens: 100, OutputTokens: 10}
	errMsg := assistant("partial", usage)
	errMsg.StopReason = agentcore.StopReasonError
	cases := []struct {
		name       string
		events     []provider.AssistantMessageEvent
		wantStatus string // "" = nothing recorded
	}{
		{"error with usage is charged", []provider.AssistantMessageEvent{
			provider.StreamTextEvent{Partial: assistant("partial", nil)},
			provider.StreamErrorEvent{Message: errMsg, Err: errors.New("boom")},
		}, usageError},
		{"rejected before output is not recorded", []provider.AssistantMessageEvent{
			provider.StreamErrorEvent{Message: agentcore.AssistantMessage{StopReason: agentcore.StopReasonError}, Err: errors.New("429")},
		}, ""},
		{"finished without usage is unreported", []provider.AssistantMessageEvent{
			provider.StreamTextEvent{Partial: assistant("ok", nil)},
			provider.StreamDoneEvent{Message: assistant("ok", nil)},
		}, usageUnreported},
		{"ended without a terminal event after output", []provider.AssistantMessageEvent{
			provider.StreamTextEvent{Partial: assistant("ok", nil)},
		}, usageAborted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newMeterFixture(t)
			turn := f.turn("public")
			ctx := withTurn(context.Background(), turn)
			s, err := f.meter.wrap(scriptedStream(tc.events...), "deepseek", "chat")(ctx, "deepseek-chat", provider.LlmContext{}, provider.StreamConfig{})
			if err != nil {
				t.Fatal(err)
			}
			_, _ = drain(t, ctx, s)
			turn.wait(time.Second)
			entries := f.entries(t)
			if tc.wantStatus == "" {
				if len(entries) != 0 {
					t.Errorf("recorded %+v, want nothing", entries)
				}
				return
			}
			if len(entries) != 1 || entries[0].Status != tc.wantStatus {
				t.Fatalf("entries = %+v, want one %q", entries, tc.wantStatus)
			}
			if entries[0].BilledTo != billedPlatform {
				t.Errorf("billedTo = %q", entries[0].BilledTo)
			}
		})
	}
}

// TestMeterAbortedByCancel: the consumer goes away mid-call. The meter still
// drains the provider stream, records the call as aborted, and does not leak.
func TestMeterAbortedByCancel(t *testing.T) {
	f := newMeterFixture(t)
	release := make(chan struct{})
	inner := func(ctx context.Context, _ string, _ provider.LlmContext, _ provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		s := provider.NewAssistantMessageEventStream(0)
		go func() {
			defer s.Close()
			if s.Emit(ctx, provider.StreamTextEvent{Partial: assistant("par", nil)}) != nil {
				return
			}
			<-release
			// A provider sees the cancellation and ends with an aborted error.
			msg := assistant("par", nil)
			msg.StopReason = agentcore.StopReasonAborted
			_ = s.Emit(ctx, provider.StreamErrorEvent{Message: msg, Err: ctx.Err()})
		}()
		return s, nil
	}
	turn := f.turn("env")
	ctx, cancel := context.WithCancel(withTurn(context.Background(), turn))
	s, err := f.meter.wrap(inner, "deepseek", "chat")(ctx, "deepseek-chat", provider.LlmContext{}, provider.StreamConfig{})
	if err != nil {
		t.Fatal(err)
	}
	<-s.Events() // the first delta arrives, then the consumer walks away
	cancel()
	close(release)
	turn.wait(2 * time.Second)
	entries := f.entries(t)
	if len(entries) != 1 || entries[0].Status != usageAborted || entries[0].Input != 0 {
		t.Fatalf("entries = %+v, want one aborted entry without counts", entries)
	}
}

// TestMeterPassesThroughWithoutTurn: calls outside a user turn are not metered
// and not wrapped.
func TestMeterPassesThroughWithoutTurn(t *testing.T) {
	f := newMeterFixture(t)
	s, err := f.meter.wrap(scriptedStream(provider.StreamDoneEvent{Message: assistant("ok", &agentcore.Usage{InputTokens: 1})}), "deepseek", "chat")(
		context.Background(), "deepseek-chat", provider.LlmContext{}, provider.StreamConfig{})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = drain(t, context.Background(), s)
	if n := len(f.entries(t)); n != 0 {
		t.Errorf("recorded %d entries without a turn", n)
	}
}

// TestMeterConcurrentCalls runs many metered calls at once (run with -race):
// every call is recorded exactly once and the ledger lines do not interleave.
func TestMeterConcurrentCalls(t *testing.T) {
	f := newMeterFixture(t)
	stream := f.meter.wrap(scriptedStream(
		provider.StreamTextEvent{Partial: assistant("ok", nil)},
		provider.StreamDoneEvent{Message: assistant("ok", &agentcore.Usage{InputTokens: 10, OutputTokens: 1})},
	), "deepseek", "chat")
	turn := f.turn("public")
	ctx := withTurn(context.Background(), turn)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := stream(ctx, "deepseek-chat", provider.LlmContext{}, provider.StreamConfig{})
			if err == nil {
				_, _ = drain(t, ctx, s)
			}
		}()
	}
	wg.Wait()
	turn.wait(2 * time.Second)
	if n := len(f.entries(t)); n != 32 {
		t.Errorf("entries = %d, want 32", n)
	}
}

// --- end to end --------------------------------------------------------------

// fakeLLM is an OpenAI-compatible endpoint that answers by what it is asked:
// a summary request gets a summary; a request whose last message is a tool
// result gets the final answer; anything else gets a tool call. Every answer
// reports usage, distinct per kind so the ledger can be checked by hand.
type fakeLLM struct {
	mu    sync.Mutex
	kinds []string
}

func (f *fakeLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	raw := new(bytes.Buffer)
	_, _ = raw.ReadFrom(r.Body)
	_ = json.Unmarshal(raw.Bytes(), &req)
	kind := "tool-call"
	switch {
	case strings.Contains(raw.String(), "checkpoint summary") || strings.Contains(raw.String(), "existing summary"):
		kind = "summary"
	case len(req.Messages) > 0 && req.Messages[len(req.Messages)-1].Role == "tool":
		kind = "answer"
	}
	f.mu.Lock()
	f.kinds = append(f.kinds, kind)
	f.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	chunk := func(s string) { fmt.Fprintf(w, "data: %s\n\n", s) }
	switch kind {
	case "tool-call":
		chunk(`{"id":"r-tool","model":"fake-1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"nosuchtool","arguments":"{}"}}]}}]}`)
		chunk(`{"id":"r-tool","model":"fake-1","choices":[{"finish_reason":"tool_calls","delta":{}}]}`)
		chunk(`{"id":"r-tool","model":"fake-1","choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":600}}}`)
	case "answer":
		chunk(`{"id":"r-answer","model":"fake-1","choices":[{"delta":{"content":"done"}}]}`)
		chunk(`{"id":"r-answer","model":"fake-1","choices":[{"finish_reason":"stop","delta":{}}]}`)
		chunk(`{"id":"r-answer","model":"fake-1","choices":[],"usage":{"prompt_tokens":1100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":1000}}}`)
	case "summary":
		chunk(`{"id":"r-summary","model":"fake-1","choices":[{"delta":{"content":"## Goal\nsummary"}}]}`)
		chunk(`{"id":"r-summary","model":"fake-1","choices":[{"finish_reason":"stop","delta":{}}]}`)
		chunk(`{"id":"r-summary","model":"fake-1","choices":[],"usage":{"prompt_tokens":3000,"completion_tokens":100}}`)
	}
	chunk("[DONE]")
}

func (f *fakeLLM) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.kinds...)
}

// TestBillingEndToEnd drives a real streamed turn through the host loop against
// a custom OpenAI-compatible endpoint. The turn makes a tool call, answers, and
// — with a context window small enough to force it — compacts its context.
// Every call is in the ledger under the one turn, priced by hand-checkable
// arithmetic, and reported to the client before "done".
func TestBillingEndToEnd(t *testing.T) {
	clearProviderKeys(t)
	llm := &fakeLLM{}
	upstream := httptest.NewServer(llm)
	defer upstream.Close()

	server := newTestServer(t)
	server.loopFn = nil
	ledger, err := newLedgerStore(server.db)
	if err != nil {
		t.Fatal(err)
	}
	server.ledger = ledger
	server.meter = &meter{ledger: ledger, settings: server.settings}
	if err := server.settings.putCustomProvider(customProvider{Name: "fakellm", Protocol: provider.ProtocolOpenAI, BaseURL: upstream.URL}); err != nil {
		t.Fatal(err)
	}
	if err := server.credentials.setPublicKey("fakellm", "sk-test"); err != nil {
		t.Fatal(err)
	}
	if err := server.settings.putPrice(modelPrice{Provider: "fakellm", Model: "fake-1", Input: 4, CacheRead: ptr(1), Output: 16}); err != nil {
		t.Fatal(err)
	}

	id := createSession(t, server)
	managed := server.sessions[id]
	managed.meta.Model = "fake-model"
	managed.meta.Provider = "fakellm"
	managed.meta.UserID = "u-1"
	if err := server.ensureHostLoop(managed); err != nil {
		t.Fatal(err)
	}
	// An earlier exchange long enough to be worth summarizing, and a window
	// small enough that the context is over it after every call.
	long := strings.Repeat("earlier context ", 400)
	managed.agentCtx.Messages = agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent(long)}},
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent(long)}, StopReason: agentcore.StopReasonEndTurn},
	}
	managed.runCfg.ContextWindow = 200
	managed.runCfg.Compaction = compaction.CompactionSettings{Enabled: true, KeepRecentTokens: 10}

	body, _ := json.Marshal(messageRequest{Prompt: "hi"})
	request := httptest.NewRequest(http.MethodPost, "/api/sessions/"+id+"/messages?stream=true", bytes.NewReader(body))
	request.SetPathValue("id", id)
	response := httptest.NewRecorder()
	server.handleMessage(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}

	calls := llm.calls()
	t.Logf("model calls: %v", calls)
	count := map[string]int{}
	for _, kind := range calls {
		count[kind]++
	}
	if count["tool-call"] != 1 || count["answer"] != 1 || count["summary"] == 0 {
		t.Fatalf("model calls = %v, want a tool call, an answer and at least one summary", calls)
	}

	var reported int
	sawDone := false
	for _, line := range strings.Split(strings.TrimSpace(response.Body.String()), "\n") {
		var ev streamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("bad line %q", line)
		}
		switch ev.Type {
		case "usage":
			if sawDone {
				t.Error("usage reported after done")
			}
			reported++
		case "done":
			sawDone = true
		case "error":
			t.Fatalf("turn failed: %s", ev.Error)
		}
	}
	if reported != len(calls) || !sawDone {
		t.Fatalf("usage events = %d for %d calls, done = %v", reported, len(calls), sawDone)
	}

	entries, err := ledger.scan(ledgerQuery{SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(calls) {
		t.Fatalf("ledger entries = %d, want one per call (%d)", len(entries), len(calls))
	}
	// Hand-computed, in yuan-tokens per million × 1000 = nano-yuan:
	//   tool call: 400 uncached × 4 + 600 cached × 1 + 20 out × 16 = 2520
	//   answer:    100 × 4 + 1000 × 1 + 5 × 16                    = 1480
	//   summary:   3000 × 4 + 100 × 16                            = 13600
	want := map[string]struct {
		kind  string
		input int
		cache int
		cost  int64
	}{
		"r-tool":    {"chat", 400, 600, 2520000},
		"r-answer":  {"chat", 100, 1000, 1480000},
		"r-summary": {"compaction", 3000, 0, 13600000},
	}
	// Reloading the conversation puts the whole turn's usage — compaction
	// included — under its reply.
	request = httptest.NewRequest(http.MethodGet, "/api/sessions/"+id+"/messages", nil)
	request.SetPathValue("id", id)
	response = httptest.NewRecorder()
	server.handleSessionMessages(response, request)
	var history []struct {
		Role    string     `json:"role"`
		Content string     `json:"content"`
		Usage   *turnUsage `json:"usage"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	var withUsage int
	for _, message := range history {
		if message.Usage == nil {
			continue
		}
		withUsage++
		if message.Content != "done" || message.Usage.Calls != len(calls) || len(message.Usage.Details) != len(calls) {
			t.Errorf("usage on %q = %+v", message.Content, message.Usage)
		}
	}
	if withUsage != 1 {
		t.Errorf("%d messages carry usage, want 1", withUsage)
	}

	for i, e := range entries {
		w, ok := want[e.ResponseID]
		if !ok || e.Kind != w.kind || e.Input != w.input || e.CacheRead != w.cache || e.Cost != w.cost || e.Status != usageOK {
			t.Errorf("entry %d = %+v, want %+v", i, e, w)
		}
		if e.UserID != "u-1" || e.Provider != "fakellm" || e.KeySource != "public" || e.BilledTo != billedPlatform || e.TurnID != entries[0].TurnID {
			t.Errorf("entry %d attribution = %+v", i, e)
		}
	}
}

// --- API -----------------------------------------------------------------------

func callJSON(t *testing.T, handler http.HandlerFunc, method, target string, body any, principal requestPrincipal) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	request := requestWithPrincipal(httptest.NewRequest(method, target, reader), principal)
	response := httptest.NewRecorder()
	handler(response, request)
	return response
}

func TestPriceAPI(t *testing.T) {
	server := newTestServer(t)
	admin := requestPrincipal{UserID: "a", Username: "root", Admin: true}

	response := callJSON(t, server.handlePutPrice, http.MethodPut, "/api/admin/prices",
		modelPrice{Provider: "OpenRouter", Model: "openrouter/auto", Input: 7, Output: 21}, admin)
	if response.Code != http.StatusOK {
		t.Fatalf("put = %d %s", response.Code, response.Body.String())
	}
	response = callJSON(t, server.handlePutPrice, http.MethodPut, "/api/admin/prices",
		modelPrice{Provider: "x", Model: "m", Input: -3}, admin)
	if response.Code != http.StatusBadRequest {
		t.Errorf("invalid price accepted: %d", response.Code)
	}

	var list priceListResponse
	response = callJSON(t, server.handleListPrices, http.MethodGet, "/api/prices", nil, requestPrincipal{UserID: "u"})
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Currency != "CNY" || len(list.Prices) != 1 {
		t.Fatalf("list = %+v", list)
	}
	row := list.Prices[0]
	if row.Provider != "openrouter" || row.UpdatedBy != "root" || row.UpdatedAt.IsZero() {
		t.Errorf("row = %+v", row)
	}

	// A model id with a slash is addressed through the query string.
	response = callJSON(t, server.handleDeletePrice, http.MethodDelete, "/api/admin/prices?provider=openrouter&model=openrouter%2Fauto", nil, admin)
	if response.Code != http.StatusOK || len(server.settings.prices()) != 0 {
		t.Errorf("delete = %d %s", response.Code, response.Body.String())
	}
	response = callJSON(t, server.handleDeletePrice, http.MethodDelete, "/api/admin/prices?provider=openrouter&model=openrouter%2Fauto", nil, admin)
	if response.Code != http.StatusNotFound {
		t.Errorf("second delete = %d", response.Code)
	}
}

func TestUsageReports(t *testing.T) {
	server := newTestServer(t)
	ledger, err := newLedgerStore(server.db)
	if err != nil {
		t.Fatal(err)
	}
	server.ledger = ledger
	alice, _, _, err := server.auth.register("alice", "password-alice")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	price := &priceSnapshot{Input: 2, CacheRead: 0.5, CacheWrite: 2, Output: 8}
	for _, e := range []ledgerEntry{
		{ID: "1", At: now, UserID: alice.ID, Username: "alice", SessionID: "s1", Provider: "deepseek", Model: "deepseek-chat", ResponseModel: "deepseek-flash",
			Kind: "chat", Status: usageOK, BilledTo: billedPlatform, Input: 205, CacheRead: 768, Output: 1, Price: price, Priced: true, Cost: 802000},
		{ID: "2", At: now, UserID: alice.ID, Username: "alice", SessionID: "s1", Provider: "deepseek", Model: "deepseek-chat",
			Kind: "chat", Status: usageOK, BilledTo: billedSelf, Input: 1000, Output: 100, Price: price, Priced: true, Cost: 2800000},
		// A user since deleted, on an unpriced model, with an aborted call.
		{ID: "3", At: now, UserID: "gone", Username: "bob", SessionID: "s2", Provider: "fakellm", Model: "free-1",
			Kind: "chat", Status: usageAborted, BilledTo: billedPlatform},
	} {
		if err := ledger.append(e); err != nil {
			t.Fatal(err)
		}
	}

	// A user sees only their own calls, whatever they ask for.
	var mine usageReportResponse
	response := callJSON(t, server.handleMyUsage, http.MethodGet, "/api/usage", nil, requestPrincipal{UserID: alice.ID, Username: "alice"})
	if err := json.Unmarshal(response.Body.Bytes(), &mine); err != nil {
		t.Fatalf("%v: %s", err, response.Body.String())
	}
	if mine.Totals.Calls != 2 || mine.Totals.Cost != 0.003602 || mine.Totals.PlatformCost != 0.000802 || mine.Totals.SelfCost != 0.0028 {
		t.Errorf("my totals = %+v", mine.Totals)
	}
	if len(mine.ByUser) != 0 {
		t.Error("personal report breaks down by user")
	}
	if len(mine.ByModel) != 2 || mine.ByModel[0].Key != "deepseek/deepseek-chat" {
		t.Errorf("by model = %+v", mine.ByModel)
	}

	var all usageReportResponse
	response = callJSON(t, server.handleAdminUsage, http.MethodGet, "/api/admin/usage", nil, requestPrincipal{UserID: "a", Admin: true})
	if err := json.Unmarshal(response.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if all.Totals.Calls != 3 || all.Totals.Unpriced != 1 || all.Totals.Incomplete != 1 {
		t.Errorf("all totals = %+v", all.Totals)
	}
	if len(all.ByUser) != 2 {
		t.Fatalf("by user = %+v", all.ByUser)
	}
	for _, row := range all.ByUser {
		if row.Key == "gone" && (!row.Deleted || row.Label != "bob") {
			t.Errorf("deleted user row = %+v", row)
		}
		if row.Key == alice.ID && row.Deleted {
			t.Error("live user marked deleted")
		}
	}
	if len(all.Unpriced) != 1 || all.Unpriced[0].Model != "free-1" {
		t.Errorf("unpriced = %+v", all.Unpriced)
	}
	if len(all.Recent) != 3 || all.Recent[0].CostYuan != 0 {
		t.Errorf("recent = %+v", all.Recent)
	}

	response = callJSON(t, server.handleAdminUsage, http.MethodGet, "/api/admin/usage?format=csv&user=gone", nil, requestPrincipal{UserID: "a", Admin: true})
	body := response.Body.String()
	if !strings.HasPrefix(body, "\ufeff时间,") || strings.Count(strings.TrimSpace(body), "\n") != 1 || !strings.Contains(body, "bob") {
		t.Errorf("csv = %q", body)
	}

	response = callJSON(t, server.handleMyUsage, http.MethodGet, "/api/usage?from=2026-13-01", nil, requestPrincipal{UserID: alice.ID})
	if response.Code != http.StatusBadRequest {
		t.Errorf("bad date = %d", response.Code)
	}
}

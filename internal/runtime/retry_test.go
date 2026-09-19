package runtime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/provider"
)

// errorAssistant is the terminal assistant message a failed attempt produces.
func errorAssistant(msg string) agentcore.AssistantMessage {
	return agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonError, ErrorMessage: msg}
}

func okAssistant(text string) agentcore.AssistantMessage {
	return agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		StopReason: agentcore.StopReasonEndTurn,
		Content:    agentcore.ContentList{agentcore.NewTextContent(text)},
	}
}

// fastRetry keeps retry-enabled tests instant while leaving MaxRetries at its
// default (3), which is what the tests below assert against.
var fastRetry = RetrySettings{BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}

// failThenSucceed returns a StreamFn that fails its first len(failures) calls,
// each as a terminal StreamErrorEvent carrying the structured cause (the
// provider contract for a runtime failure), then succeeds with success.
func failThenSucceed(failures []error, success agentcore.AssistantMessage) provider.StreamFn {
	call := 0
	return func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		i := call
		call++
		s := provider.NewAssistantMessageEventStream(0)
		go func() {
			defer s.Close()
			// Like a real provider, the request opens with an empty partial: it
			// reaches the context via back-fill, so a retry must unwind it.
			_ = s.Emit(ctx, provider.StreamStartEvent{Partial: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}})
			if i < len(failures) {
				_ = s.Emit(ctx, provider.StreamErrorEvent{
					Message: errorAssistant(failures[i].Error()),
					Err:     failures[i],
				})
				return
			}
			_ = s.Emit(ctx, provider.StreamDoneEvent{Message: success})
		}()
		return s, nil
	}
}

// repeatErr is n copies of err, for a stream that never recovers.
func repeatErr(n int, err error) []error {
	out := make([]error, n)
	for i := range out {
		out[i] = err
	}
	return out
}

func rateLimited() error { return &provider.UpstreamError{Status: 429} }

// --- policy -----------------------------------------------------------------

// TestRetryBudgetClassification verifies the budget defers to the provider's
// own classification: structure (status code, error type), never message text.
func TestRetryBudgetClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"rate limit", &provider.UpstreamError{Status: 429}, true},
		{"overloaded", &provider.UpstreamError{Status: 529}, true},
		{"server error", &provider.UpstreamError{Status: 500, Body: `{"error":"invalid state"}`}, true},
		{"unauthorized", &provider.UpstreamError{Status: 401}, false},
		{"unknown model", &provider.UpstreamError{Status: 404, Body: "no endpoints found, try again later"}, false},
		{"provider overloaded", &provider.APIError{Family: "anthropic", Type: "overloaded_error", Message: "Overloaded"}, true},
		{"provider rejected", &provider.APIError{Family: "anthropic", Type: "invalid_request_error", Message: "bad tool"}, false},
		{"unclassified", errors.New("boom"), false},
		{"cancelled", context.Canceled, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, ok := newRetryBudget(fastRetry).next(tc.err)
			if ok != tc.want {
				t.Errorf("retryable = %v, want %v for %v", ok, tc.want, tc.err)
			}
		})
	}
}

// TestRetryBudgetBackoff verifies the doubling sequence and the cap.
func TestRetryBudgetBackoff(t *testing.T) {
	b := newRetryBudget(RetrySettings{})
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, 60 * time.Second, 60 * time.Second}
	for i, w := range want {
		if got := b.backoff(i + 1); got != w {
			t.Errorf("backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}

// TestRetryBudgetDefaults verifies the zero value is "on, with defaults" and
// that each numeric field falls back independently.
func TestRetryBudgetDefaults(t *testing.T) {
	if got := (RetrySettings{}).withDefaults(); got != DefaultRetrySettings() {
		t.Errorf("zero value = %+v, want %+v", got, DefaultRetrySettings())
	}
	partial := RetrySettings{MaxRetries: 5}.withDefaults()
	if partial.Disabled || partial.MaxRetries != 5 || partial.BaseDelay != defaultBaseDelay || partial.MaxDelay != defaultMaxDelay {
		t.Errorf("partial override = %+v", partial)
	}
	if _, _, ok := newRetryBudget(RetrySettings{Disabled: true}).next(rateLimited()); ok {
		t.Error("Disabled must refuse every retry")
	}
	if _, _, ok := (*retryBudget)(nil).next(rateLimited()); ok {
		t.Error("a nil budget must refuse every retry")
	}
}

// TestRetryBudgetIsSpentOncePerRun verifies the allowance is shared across a
// run rather than reset per request.
func TestRetryBudgetIsSpentOncePerRun(t *testing.T) {
	b := newRetryBudget(fastRetry)
	for i := 1; i <= defaultMaxRetries; i++ {
		_, attempt, ok := b.next(rateLimited())
		if !ok || attempt != i {
			t.Fatalf("retry %d: ok=%v attempt=%d", i, ok, attempt)
		}
	}
	if _, _, ok := b.next(rateLimited()); ok {
		t.Errorf("budget of %d must be exhausted", defaultMaxRetries)
	}
}

// TestRetryBudgetHonorsRetryAfter verifies the server's hint is a floor, and
// that a hint longer than MaxDelay ends the retries instead of stalling.
func TestRetryBudgetHonorsRetryAfter(t *testing.T) {
	b := newRetryBudget(RetrySettings{}) // 2s base, 60s cap
	delay, _, ok := b.next(&provider.UpstreamError{Status: 429, RetryAfter: 9 * time.Second})
	if !ok || delay != 9*time.Second {
		t.Errorf("Retry-After must raise the backoff: delay=%v ok=%v", delay, ok)
	}
	delay, _, ok = b.next(&provider.UpstreamError{Status: 429, RetryAfter: time.Second})
	if !ok || delay != 4*time.Second {
		t.Errorf("a hint below the backoff must not lower it: delay=%v ok=%v", delay, ok)
	}
	if _, _, ok := b.next(&provider.UpstreamError{Status: 429, RetryAfter: 2 * time.Hour}); ok {
		t.Error("a hint beyond MaxDelay must end the retries, not sleep the cap")
	}
}

// --- run behavior -----------------------------------------------------------

// TestRunRetriesTransientFailureThenSucceeds is the core guarantee: a retried
// attempt is invisible. The run reports exactly one turn, one message_end and
// one message — the successful one — with the failed attempts unwound out of
// the context entirely.
func TestRunRetriesTransientFailureThenSucceeds(t *testing.T) {
	cfg := newRunCfg(failThenSucceed(repeatErr(2, rateLimited()), okAssistant("ok")))
	cfg.Retry = fastRetry
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{agentcore.UserMessage{RoleField: agentcore.RoleUser}}}

	kinds, msgs := collectStream(t, agentLoop(context.Background(), agentCtx, cfg))

	assertEventKinds(t, kinds, []string{
		agentcore.EventAgentStart,
		agentcore.EventTurnStart,
		agentcore.EventMessageStart, agentcore.EventRetry,
		agentcore.EventMessageStart, agentcore.EventRetry,
		agentcore.EventMessageStart, agentcore.EventMessageEnd,
		agentcore.EventTurnEnd,
		agentcore.EventTelemetry,
		agentcore.EventAgentEnd,
	})
	if len(msgs) != 1 {
		t.Fatalf("run produced %d messages, want only the successful one: %+v", len(msgs), msgs)
	}
	final, ok := msgs[0].(agentcore.AssistantMessage)
	if !ok || agentcore.ContentToText(final.Content) != "ok" {
		t.Fatalf("final message = %+v, want the successful response", msgs[0])
	}
	if got := len(agentCtx.Messages); got != 2 {
		t.Errorf("context holds %d messages, want 2 (user + success): %+v", got, agentCtx.Messages)
	}
}

// TestRunReportsRetriesInTelemetry verifies retries ride the existing telemetry
// summary rather than needing a side channel.
func TestRunReportsRetriesInTelemetry(t *testing.T) {
	cfg := newRunCfg(failThenSucceed(repeatErr(2, rateLimited()), okAssistant("ok")))
	cfg.Retry = fastRetry
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{agentcore.UserMessage{RoleField: agentcore.RoleUser}}}

	var tel agentcore.TelemetryEvent
	for ev := range agentLoop(context.Background(), agentCtx, cfg).Events() {
		if e, ok := ev.(agentcore.TelemetryEvent); ok {
			tel = e
		}
	}
	if tel.RetryCount != 2 {
		t.Errorf("telemetry RetryCount = %d, want 2", tel.RetryCount)
	}
	if tel.Turns != 1 {
		t.Errorf("telemetry Turns = %d, want 1: a retry is not a turn", tel.Turns)
	}
}

// TestRunGivesUpAfterBudget verifies an unrecoverable transient failure ends
// the run after the budget, surfacing the last failure as the turn's message.
func TestRunGivesUpAfterBudget(t *testing.T) {
	cfg := newRunCfg(failThenSucceed(repeatErr(10, rateLimited()), okAssistant("unreachable")))
	cfg.Retry = fastRetry
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{agentcore.UserMessage{RoleField: agentcore.RoleUser}}}

	kinds, msgs := collectStream(t, agentLoop(context.Background(), agentCtx, cfg))

	if got := countKind(kinds, agentcore.EventRetry); got != defaultMaxRetries {
		t.Errorf("retry events = %d, want %d", got, defaultMaxRetries)
	}
	if got := countKind(kinds, agentcore.EventTurnStart); got != 1 {
		t.Errorf("turn starts = %d, want 1", got)
	}
	assertFailedRun(t, msgs)
}

// TestRunDoesNotRetryPermanentFailure verifies a rejection fails immediately.
func TestRunDoesNotRetryPermanentFailure(t *testing.T) {
	cfg := newRunCfg(failThenSucceed(
		[]error{&provider.UpstreamError{Status: 401, Body: "unauthorized"}},
		okAssistant("unreachable"),
	))
	cfg.Retry = fastRetry
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{agentcore.UserMessage{RoleField: agentcore.RoleUser}}}

	kinds, msgs := collectStream(t, agentLoop(context.Background(), agentCtx, cfg))

	if got := countKind(kinds, agentcore.EventRetry); got != 0 {
		t.Errorf("retry events = %d, want 0", got)
	}
	assertFailedRun(t, msgs)
}

// TestRunRetryDisabled verifies the opt-out reaches the request boundary.
func TestRunRetryDisabled(t *testing.T) {
	cfg := newRunCfg(failThenSucceed(repeatErr(1, rateLimited()), okAssistant("unreachable")))
	cfg.Retry = RetrySettings{Disabled: true}
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{agentcore.UserMessage{RoleField: agentcore.RoleUser}}}

	kinds, msgs := collectStream(t, agentLoop(context.Background(), agentCtx, cfg))

	if got := countKind(kinds, agentcore.EventRetry); got != 0 {
		t.Errorf("retry events = %d, want 0 when disabled", got)
	}
	assertFailedRun(t, msgs)
}

// TestRunDoesNotRetryAfterVisibleOutput verifies the invisibility rule: once a
// partial has reached consumers the attempt is committed, because replacing it
// would rewrite text the user has already seen.
func TestRunDoesNotRetryAfterVisibleOutput(t *testing.T) {
	partial := agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent("half")}}
	cfg := newRunCfg(func(ctx context.Context, model string, llm provider.LlmContext, scfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		s := provider.NewAssistantMessageEventStream(0)
		go func() {
			defer s.Close()
			_ = s.Emit(ctx, provider.StreamStartEvent{Partial: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}})
			_ = s.Emit(ctx, provider.StreamTextEvent{Partial: partial})
			_ = s.Emit(ctx, provider.StreamErrorEvent{Message: errorAssistant("connection reset by peer"), Err: rateLimited()})
		}()
		return s, nil
	})
	cfg.Retry = fastRetry
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{agentcore.UserMessage{RoleField: agentcore.RoleUser}}}

	kinds, msgs := collectStream(t, agentLoop(context.Background(), agentCtx, cfg))

	if got := countKind(kinds, agentcore.EventRetry); got != 0 {
		t.Errorf("retry events = %d, want 0 once output is visible", got)
	}
	assertFailedRun(t, msgs)
}

// TestRunRetryCancelDuringBackoff verifies Ctrl+C during the wait ends the run
// promptly and still reports the failure it was retrying.
func TestRunRetryCancelDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := newRunCfg(failThenSucceed(repeatErr(10, rateLimited()), okAssistant("unreachable")))
	cfg.Retry = RetrySettings{MaxRetries: 10, BaseDelay: time.Hour, MaxDelay: time.Hour}
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{agentcore.UserMessage{RoleField: agentcore.RoleUser}}}

	stream := agentLoop(ctx, agentCtx, cfg)

	// Cancelling on the retry event is the deterministic trigger: receiving it
	// means the loop has announced the wait and is entering it. The event slice
	// never leaves this goroutine, so there is nothing to race on.
	done := make(chan []string, 1)
	go func() {
		var kinds []string
		for ev := range stream.Events() {
			kinds = append(kinds, ev.EventType())
			if ev.EventType() == agentcore.EventRetry {
				cancel()
			}
		}
		done <- kinds
	}()

	select {
	case kinds := <-done:
		if got := countKind(kinds, agentcore.EventRetry); got != 1 {
			t.Errorf("retry events = %d, want the one announced before the wait", got)
		}
		if got := countKind(kinds, agentcore.EventTurnStart); got != 1 {
			t.Errorf("turn starts = %d, want 1: the cancelled backoff must not start another", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not terminate after cancel during retry backoff")
	}
}

// assertFailedRun verifies the run ended on a failed assistant turn, which is
// how the loop reports failure (never through the stream result error).
func assertFailedRun(t *testing.T, msgs []agentcore.AgentMessage) {
	t.Helper()
	if len(msgs) == 0 {
		t.Fatal("run produced no messages")
	}
	last, ok := msgs[len(msgs)-1].(agentcore.AssistantMessage)
	if !ok || last.StopReason != agentcore.StopReasonError {
		t.Fatalf("final message = %+v, want a failed assistant turn", msgs[len(msgs)-1])
	}
}

// --- end to end ---------------------------------------------------------------

// openAISuccessSSE is a minimal OpenAI-compatible completion.
const openAISuccessSSE = `data: {"id":"c1","model":"m","choices":[{"delta":{"role":"assistant"}}]}

data: {"id":"c1","model":"m","choices":[{"delta":{"content":"recovered"}}]}

data: {"id":"c1","model":"m","choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`

// TestRunRetriesAgainstRealTransport drives the whole chain — the OpenRouter
// driver, the HTTP transport, the typed error, the budget — against a server
// that refuses the first two requests. The refusal body is deliberately full of
// digits ("...4000 ms"), the shape that defeats substring classification.
func TestRunRetriesAgainstRealTransport(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) <= 2 {
			// 500 rather than 429 so the transport's own connect retries stay out
			// of the way: this test is about the agent-level policy.
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"upstream busy, retry after 4000 ms","code":500}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, openAISuccessSSE)
	}))
	defer srv.Close()

	p := provider.NewOpenRouterProvider(srv.URL, []provider.Model{{Provider: "openrouter", ID: "m"}})
	cfg := newRunCfg(provider.StreamFnFromProvider(p))
	cfg.Model, cfg.APIKey, cfg.Retry = "m", "sk-test", fastRetry
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("hi")}}}}

	kinds, msgs := collectStream(t, agentLoop(context.Background(), agentCtx, cfg))

	if got := countKind(kinds, agentcore.EventRetry); got != 2 {
		t.Errorf("retry events = %d, want 2 (kinds %v)", got, kinds)
	}
	if got := requests.Load(); got != 3 {
		t.Errorf("upstream requests = %d, want 3", got)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want only the successful one: %+v", len(msgs), msgs)
	}
	final := msgs[0].(agentcore.AssistantMessage)
	if agentcore.ContentToText(final.Content) != "recovered" {
		t.Errorf("final text = %q, want %q", agentcore.ContentToText(final.Content), "recovered")
	}
}

// TestRunFailsFastAgainstRealTransport is the same chain for a rejection: a 401
// must cost exactly one request. It asserts through DrainStream, the path the
// REPL and TUI use, because a failure this early never produced a stream and so
// (as before this change) leaves no message in the context — it is reported
// only as the turn's terminal message.
func TestRunFailsFastAgainstRealTransport(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid key, please try again","code":401}}`)
	}))
	defer srv.Close()

	p := provider.NewOpenRouterProvider(srv.URL, []provider.Model{{Provider: "openrouter", ID: "m"}})
	cfg := newRunCfg(provider.StreamFnFromProvider(p))
	cfg.Model, cfg.APIKey, cfg.Retry = "m", "sk-test", fastRetry
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("hi")}}}}

	retries := 0
	final, err := DrainStream(context.Background(), agentLoop(context.Background(), agentCtx, cfg), StreamHandler{
		OnEvent: func(ev agentcore.AgentEvent) {
			if _, ok := ev.(agentcore.RetryEvent); ok {
				retries++
			}
		},
	})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if retries != 0 {
		t.Errorf("retry events = %d, want 0 for a rejected key", retries)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("upstream requests = %d, want 1", got)
	}
	if final == nil || final.StopReason != agentcore.StopReasonError {
		t.Fatalf("final turn = %+v, want a failed one", final)
	}
	if !strings.Contains(final.ErrorMessage, "401") {
		t.Errorf("error message = %q, want it to name the status", final.ErrorMessage)
	}
}

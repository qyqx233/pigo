package provider

import (
	"encoding/json"
	"testing"

	"github.com/openai/openai-go/responses"

	"github.com/smallnest/pigo/internal/agentcore"
)

// The payloads below are real responses captured from DeepSeek on 2026-09-19:
// the same 973-token prompt sent twice, so the second call hits the cache.
const (
	deepseekChatColdUsage = `{"prompt_tokens":973,"completion_tokens":1,"total_tokens":974,"prompt_tokens_details":{"cached_tokens":0},"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":973}`
	deepseekChatWarmUsage = `{"prompt_tokens":973,"completion_tokens":1,"total_tokens":974,"prompt_tokens_details":{"cached_tokens":768},"prompt_cache_hit_tokens":768,"prompt_cache_miss_tokens":205}`
	deepseekResponsesBody = `{"id":"93fc9aaa","object":"response","model":"deepseek-flash","status":"completed",
		"output":[{"type":"message","id":"m1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[]}]}],
		"usage":{"input_tokens":973,"input_tokens_details":{"cached_tokens":768},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":974}}`
)

// decodeOpenAIUsage runs a usage-only chunk through the real Chat Completions
// decoder and returns what reached the finished message.
func decodeOpenAIUsage(t *testing.T, usageJSON string) agentcore.Usage {
	t.Helper()
	dec := NewOpenAIDecoder()
	if _, err := dec.Decode([]byte(`{"id":"c1","model":"m","choices":[{"delta":{"content":"ok"}}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := dec.Decode([]byte(`{"id":"c1","model":"m","choices":[],"usage":` + usageJSON + `}`)); err != nil {
		t.Fatal(err)
	}
	events, err := dec.Finish()
	if err != nil {
		t.Fatal(err)
	}
	done, ok := events[len(events)-1].(StreamDoneEvent)
	if !ok || done.Message.Usage == nil {
		t.Fatalf("no usage on the finished message: %+v", events)
	}
	return *done.Message.Usage
}

// TestOpenAIDialectDeepSeekChat uses the captured DeepSeek responses. DeepSeek
// sends both the standard and its own cache fields; they agree, and the total
// must be split rather than counted twice.
func TestOpenAIDialectDeepSeekChat(t *testing.T) {
	cold := decodeOpenAIUsage(t, deepseekChatColdUsage)
	if cold.InputTokens != 973 || cold.CacheReadTokens != 0 || cold.OutputTokens != 1 {
		t.Errorf("cold = %+v", cold)
	}
	warm := decodeOpenAIUsage(t, deepseekChatWarmUsage)
	if warm.InputTokens != 205 || warm.CacheReadTokens != 768 || warm.OutputTokens != 1 {
		t.Errorf("warm = %+v, want 205 uncached + 768 cached", warm)
	}
	// Splitting must not change the size of the prompt.
	if warm.TotalInputTokens() != 973 {
		t.Errorf("total input = %d, want 973", warm.TotalInputTokens())
	}
}

// TestOpenAIDialectVendorFallbacks covers each place a vendor may report cache
// hits, and the order they are consulted in.
func TestOpenAIDialectVendorFallbacks(t *testing.T) {
	cases := []struct {
		name       string
		usage      string
		wantInput  int
		wantCached int
	}{
		{"OpenAI standard", `{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":60}}`, 40, 60},
		{"DeepSeek native only", `{"prompt_tokens":100,"completion_tokens":5,"prompt_cache_hit_tokens":30,"prompt_cache_miss_tokens":70}`, 70, 30},
		{"Moonshot top level", `{"prompt_tokens":100,"completion_tokens":5,"cached_tokens":25}`, 75, 25},
		{"no cache reported", `{"prompt_tokens":100,"completion_tokens":5}`, 100, 0},
		// The standard field wins when a gateway sends more than one.
		{"standard wins", `{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":60},"cached_tokens":10}`, 40, 60},
		// Reported as zero is still "reported": it must not fall through to a
		// later field.
		{"explicit zero", `{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":0},"cached_tokens":50}`, 100, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeOpenAIUsage(t, tc.usage)
			if got.InputTokens != tc.wantInput || got.CacheReadTokens != tc.wantCached {
				t.Errorf("got input %d cached %d, want %d / %d", got.InputTokens, got.CacheReadTokens, tc.wantInput, tc.wantCached)
			}
			if got.Anomaly {
				t.Error("a well-formed response must not be flagged")
			}
		})
	}
}

// TestOpenAIDialectReasoning verifies reasoning is reported inside output.
func TestOpenAIDialectReasoning(t *testing.T) {
	got := decodeOpenAIUsage(t, `{"prompt_tokens":10,"completion_tokens":50,"completion_tokens_details":{"reasoning_tokens":40}}`)
	if got.OutputTokens != 50 || got.ReasoningTokens != 40 {
		t.Errorf("output %d reasoning %d, want 50 / 40 (reasoning is inside output)", got.OutputTokens, got.ReasoningTokens)
	}
}

// TestOpenAIDialectAnomaly covers a vendor whose cache count cannot be a subset
// of its total: the total is kept, nothing goes negative, and the call is
// flagged.
func TestOpenAIDialectAnomaly(t *testing.T) {
	got := decodeOpenAIUsage(t, `{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":400}}`)
	if got.InputTokens != 100 || got.CacheReadTokens != 0 || !got.Anomaly {
		t.Errorf("got %+v, want the total kept, no cache, flagged", got)
	}
}

// TestResponsesDialectDeepSeek uses the captured DeepSeek Responses payload,
// which is also inclusive.
func TestResponsesDialectDeepSeek(t *testing.T) {
	var resp responses.Response
	if err := json.Unmarshal([]byte(deepseekResponsesBody), &resp); err != nil {
		t.Fatal(err)
	}
	msg := (&responsesDriver{name: "deepseek"}).mapResponse(&resp)
	if msg.Usage == nil {
		t.Fatal("no usage mapped")
	}
	u := *msg.Usage
	if u.InputTokens != 205 || u.CacheReadTokens != 768 || u.OutputTokens != 1 {
		t.Errorf("usage = %+v, want 205 uncached + 768 cached", u)
	}
	if msg.ResponseModel != "deepseek-flash" {
		t.Errorf("response model = %q; the alias must be preserved for pricing", msg.ResponseModel)
	}
}

// TestAnthropicDialect feeds the documented Messages stream: input excludes the
// cache, cache reads and writes are reported beside it, and message_delta
// carries the final output without erasing the input side.
func TestAnthropicDialect(t *testing.T) {
	dec := NewAnthropicDecoder()
	for _, payload := range []string{
		`{"type":"message_start","message":{"id":"msg_1","model":"claude-x","usage":{"input_tokens":120,"cache_read_input_tokens":2000,"cache_creation_input_tokens":300,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`,
	} {
		if _, err := dec.Decode([]byte(payload)); err != nil {
			t.Fatal(err)
		}
	}
	events, err := dec.Decode([]byte(`{"type":"message_stop"}`))
	if err != nil {
		t.Fatal(err)
	}
	done := events[len(events)-1].(StreamDoneEvent)
	u := *done.Message.Usage
	if u.InputTokens != 120 || u.CacheReadTokens != 2000 || u.CacheWriteTokens != 300 || u.OutputTokens != 42 {
		t.Errorf("usage = %+v", u)
	}
	if u.TotalInputTokens() != 2420 {
		t.Errorf("total input = %d, want 2420", u.TotalInputTokens())
	}
}

// TestNoUsageStaysNil verifies a response without accounting has no Usage, so
// "not reported" is distinguishable from "zero".
func TestNoUsageStaysNil(t *testing.T) {
	dec := NewOpenAIDecoder()
	if _, err := dec.Decode([]byte(`{"id":"c1","model":"m","choices":[{"delta":{"content":"ok"}}]}`)); err != nil {
		t.Fatal(err)
	}
	events, err := dec.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if u := events[len(events)-1].(StreamDoneEvent).Message.Usage; u != nil {
		t.Errorf("usage = %+v, want nil", *u)
	}
}

// This file converts each wire dialect's usage block into agentcore.Usage.
//
// The dialect — not the model — decides what the numbers mean. The same model
// reports differently through different endpoints (DeepSeek's chat and
// responses APIs use different field names), through a proxy (OpenRouter
// re-shapes whatever it forwards), and under an alias (a request for
// deepseek-chat can come back as deepseek-flash). So each decoder owns the raw
// shape of its own wire format and converts it here, and nothing downstream
// ever sees a vendor-specific number.
//
// Two conventions exist for "input":
//
//   - inclusive (OpenAI Chat, OpenAI Responses and everything compatible): the
//     input total already contains the cache hits, which are reported as a
//     subset of it;
//   - exclusive (Anthropic Messages): input excludes cache reads and cache
//     writes, which are reported alongside it.
//
// agentcore.Usage is exclusive, so inclusive dialects subtract.
package provider

import "github.com/smallnest/pigo/internal/agentcore"

// inclusiveUsage converts a dialect whose input total includes cache hits.
//
// A cache count larger than the total cannot be a subset of it — some vendor
// is reporting in a way we have not met. Rather than produce a negative input,
// the total is kept as-is, the cache count is dropped, and the result is
// flagged so the call can be found later instead of trusted.
func inclusiveUsage(totalInput, cached, output, reasoning int) agentcore.Usage {
	usage := agentcore.Usage{
		InputTokens:     totalInput,
		OutputTokens:    output,
		ReasoningTokens: reasoning,
	}
	switch {
	case cached < 0 || cached > totalInput:
		usage.Anomaly = cached != 0
	default:
		usage.InputTokens = totalInput - cached
		usage.CacheReadTokens = cached
	}
	return usage
}

// openaiUsage is the Chat Completions usage block, as a superset of the
// vendor variants met on that wire. Pointer fields distinguish "reported as
// zero" from "not reported", which is what the cache-count chain relies on.
type openaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	// OpenAI's standard place for cache hits; also used by OpenRouter,
	// DeepSeek and Zhipu.
	PromptTokensDetails *struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	// DeepSeek's own field, sent alongside the standard one.
	PromptCacheHitTokens *int `json:"prompt_cache_hit_tokens"`
	// Moonshot/Kimi report cache hits at the top level of usage.
	CachedTokens            *int `json:"cached_tokens"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	// OpenRouter's charge for the call, in USD, when the request asked for it.
	Cost *float64 `json:"cost"`
}

// cacheHits returns the cache-hit count from the first field that reports one,
// in the order vendors are known to use them. It is decided by what the
// response contains, not by which provider we think we called, so proxies and
// custom endpoints need no configuration.
func (u openaiUsage) cacheHits() int {
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens != nil {
		return *u.PromptTokensDetails.CachedTokens
	}
	if u.PromptCacheHitTokens != nil {
		return *u.PromptCacheHitTokens
	}
	if u.CachedTokens != nil {
		return *u.CachedTokens
	}
	return 0
}

// canonical converts the block into agentcore.Usage.
func (u openaiUsage) canonical() agentcore.Usage {
	reasoning := 0
	if u.CompletionTokensDetails != nil {
		reasoning = u.CompletionTokensDetails.ReasoningTokens
	}
	usage := inclusiveUsage(u.PromptTokens, u.cacheHits(), u.CompletionTokens, reasoning)
	usage.UpstreamCostUSD = u.Cost
	return usage
}

// anthropicUsage is the Messages usage block. Anthropic's input_tokens already
// excludes the cache, so the fields map across directly.
type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// merge folds a later usage block into the running one. message_start carries
// the input side and message_delta the cumulative output (and, on some
// deployments, the input side again); a field reported as zero later never
// erases a value seen earlier.
func (u *anthropicUsage) merge(next anthropicUsage) {
	if next.InputTokens != 0 {
		u.InputTokens = next.InputTokens
	}
	if next.OutputTokens != 0 {
		u.OutputTokens = next.OutputTokens
	}
	if next.CacheReadInputTokens != 0 {
		u.CacheReadInputTokens = next.CacheReadInputTokens
	}
	if next.CacheCreationInputTokens != 0 {
		u.CacheCreationInputTokens = next.CacheCreationInputTokens
	}
}

// canonical converts the block into agentcore.Usage.
func (u anthropicUsage) canonical() agentcore.Usage {
	return agentcore.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
}

// usagePtr returns nil for a response that carried no accounting, so an
// assistant message only has a Usage when the provider reported one.
func usagePtr(u agentcore.Usage) *agentcore.Usage {
	if u.IsZero() && u.UpstreamCostUSD == nil {
		return nil
	}
	return &u
}

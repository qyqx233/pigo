// This file implements streamAssistantResponse (US-003): it shapes the context
// into a provider request, resolves the API key dynamically, drives the
// provider stream, and back-fills the partial assistant message into the
// context while emitting message_start / message_update / message_end events.
package runtime

import (
	"context"
	"errors"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/compaction"
	"github.com/smallnest/pigo/internal/provider"
)

// LoopConfig holds the pluggable behavior of the agent loop. Every hook is
// optional (nil = use the default). The pointer/func-field pattern mirrors pi's
// optional callbacks.
type LoopConfig struct {
	// Model is the model id passed to StreamFn.
	Model string
	// APIKey is the static fallback key when GetAPIKey is nil or returns "".
	APIKey string
	// ThinkingLevel is the reasoning effort for requests.
	ThinkingLevel agentcore.ThinkingLevel
	// Stream produces the provider stream. Required (defaults are wired by
	// callers/tests, e.g. a fake provider).
	Stream provider.StreamFn

	// TransformContext optionally rewrites the message list before conversion
	// (context trimming/injection). Contract: must not error; on failure return
	// a safe fallback. Runs first.
	TransformContext func(ctx context.Context, msgs agentcore.MessageList) agentcore.MessageList
	// ConvertToLlm optionally filters UI-only messages. Defaults to identity.
	// Contract: must not error.
	ConvertToLlm func(msgs agentcore.MessageList) agentcore.MessageList
	// GetAPIKey optionally resolves a fresh key per request (handles short-lived
	// token expiry). Falls back to APIKey when nil or empty.
	GetAPIKey func(ctx context.Context, provider string) string
	// Provider is the provider name passed to GetAPIKey.
	Provider string

	// ContextWindow is the model's total context-token budget, used to decide
	// automatic compaction. When <= 0 the window is unknown and auto-compaction
	// is disabled (ShouldCompact returns false), so the loop behaves exactly as
	// before for callers that do not plumb it through.
	ContextWindow int
	// Compaction holds the thresholds/retention knobs for auto-compaction. Its
	// Enabled flag gates the feature independently of ContextWindow.
	Compaction compaction.CompactionSettings
	// SummaryStream produces the provider stream used to generate compaction
	// summaries. Defaults to Stream when nil.
	SummaryStream provider.StreamFn
	// SummaryModel is the model used for summarization. When zero, a model is
	// synthesized from Model/ContextWindow.
	SummaryModel provider.Model

	// Retry tunes agent-level retry of transient provider failures. The zero
	// value means "enabled with the defaults" (3 attempts, 2s base delay, 60s
	// cap); see RetrySettings.
	Retry RetrySettings

	// Extra is forwarded to StreamConfig.Extra.
	Extra map[string]any
}

// streamAssistantResponse runs one assistant turn: it issues the provider
// request, back-fills the response into agentCtx.Messages, and returns the
// final assistant message. It never returns an error for a request failure —
// such failures arrive as a terminal assistant message with stopReason
// error/aborted. The returned error means only that emitting was cancelled.
//
// A request that fails transiently (rate limit, overload, 5xx, a connection
// that died) is retried here rather than in the loop above, because this is the
// only layer that can make a retry invisible: it owns the back-fill into the
// context, so it can unwind the failed attempt, and it knows whether any of the
// attempt has already reached consumers. An attempt that streamed content is
// never replaced — DrainStream's delta accounting and the TUI transcript both
// assume text only ever grows within a turn, so retrying after visible output
// would duplicate or truncate it. In practice the failures this protects
// against (429, overloaded, a stream that never produced a byte) happen before
// any content arrives, which is exactly the case it covers.
//
// Whether a failure is transient is the provider layer's judgement
// (provider.IsTransient), and how many retries a run may spend is the budget's
// (retry.go). budget may be nil, which disables retry entirely.
func streamAssistantResponse(ctx context.Context, agentCtx *agentcore.AgentContext, cfg LoopConfig, budget *retryBudget, emit agentcore.EmitFunc) (agentcore.AssistantMessage, error) {
	// mark is where this turn's messages begin, so a retried attempt can be
	// unwound out of the context completely.
	mark := len(agentCtx.Messages)
	for {
		res, err := attemptAssistantResponse(ctx, agentCtx, cfg, emit)
		if err != nil {
			return agentcore.AssistantMessage{}, err
		}
		if res.Failure != nil && !res.Streamed {
			if delay, attempt, ok := budget.next(res.Failure); ok {
				if err := emit(ctx, agentcore.RetryEvent{
					Attempt:    attempt,
					MaxRetries: budget.maxRetries(),
					Delay:      delay,
					Reason:     res.Failure.Error(),
				}); err != nil {
					return agentcore.AssistantMessage{}, err
				}
				if sleepContext(ctx, delay) {
					// Drop the failed attempt only once we are certain another one
					// follows: cancelled during the backoff, the run still needs the
					// failure it already has.
					agentCtx.Messages = agentCtx.Messages[:mark]
					continue
				}
			}
		}
		// The attempt is final — success or failure, this is the message the turn
		// produced. message_end is emitted here rather than inside the attempt so
		// a retried attempt never reaches consumers at all.
		if err := emit(ctx, agentcore.MessageEndEvent{Message: res.Message}); err != nil {
			return agentcore.AssistantMessage{}, err
		}
		return res.Message, nil
	}
}

// attemptResult is the outcome of a single provider request.
type attemptResult struct {
	// Message is the assistant message the attempt produced: the completed
	// response, or the terminal error message when Failure is set.
	Message agentcore.AssistantMessage
	// Failure is the structured cause when the attempt failed, nil on success.
	// It is the provider's own error value, so provider.IsTransient can read the
	// HTTP status / error type out of it rather than out of rendered text.
	Failure error
	// Streamed reports whether any of this attempt's content already reached
	// consumers as a message_update.
	Streamed bool
}

// attemptAssistantResponse issues one provider request: it builds the request
// from agentCtx, streams the response, and back-fills the partial into
// agentCtx.Messages, emitting message_start / message_update as it goes. The
// sequence (transformContext → convertToLlm → resolve key → stream → drain) is
// kept identical to pi. The terminal message_end is left to the caller, which
// is what makes a retried attempt invisible. The returned error means only that
// emitting was cancelled.
func attemptAssistantResponse(ctx context.Context, agentCtx *agentcore.AgentContext, cfg LoopConfig, emit agentcore.EmitFunc) (attemptResult, error) {
	// 1. transformContext (optional, must not error).
	msgs := agentCtx.Messages
	if cfg.TransformContext != nil {
		msgs = cfg.TransformContext(ctx, msgs)
	}
	// 2. convertToLlm (filter UI-only; default identity).
	if cfg.ConvertToLlm != nil {
		msgs = cfg.ConvertToLlm(msgs)
	}
	// 3. shape the LLM context.
	llm := provider.LlmContext{
		SystemPrompt: agentCtx.SystemPrompt,
		Messages:     msgs,
		Tools:        agentCtx.Tools,
	}
	// 4. resolve API key dynamically, fall back to static.
	key := cfg.APIKey
	if cfg.GetAPIKey != nil {
		if dyn := cfg.GetAPIKey(ctx, cfg.Provider); dyn != "" {
			key = dyn
		}
	}
	// 5. build the provider stream.
	stream, err := cfg.Stream(ctx, cfg.Model, llm, provider.StreamConfig{
		APIKey:        key,
		ThinkingLevel: cfg.ThinkingLevel,
		Extra:         cfg.Extra,
	})
	if err != nil {
		// Early "cannot build stream" failure: synthesize a terminal message so
		// the loop has a uniform assistant message to record. Nothing was
		// back-filled, so a retry of this attempt is free.
		return attemptResult{Message: newErrorAssistantMessage(cfg, err), Failure: err}, nil
	}

	// 6. drain the stream, back-filling the partial into the context.
	addedPartial := false
	backfill := func(partial agentcore.AssistantMessage) {
		if !addedPartial {
			agentCtx.Messages = append(agentCtx.Messages, partial)
			addedPartial = true
		} else {
			agentCtx.Messages[len(agentCtx.Messages)-1] = partial
		}
	}
	// streamed latches once a partial with content has been handed to consumers:
	// from that point the attempt is visible and can no longer be replaced.
	streamed := false
	update := func(partial agentcore.AssistantMessage, ev provider.AssistantMessageEvent) error {
		backfill(partial)
		if len(partial.Content) > 0 {
			streamed = true
		}
		return emit(ctx, agentcore.MessageUpdateEvent{Message: partial, AssistantMessageEvent: ev})
	}

	for ev := range stream.Events() {
		switch e := ev.(type) {
		case provider.StreamStartEvent:
			backfill(e.Partial)
			if len(e.Partial.Content) > 0 {
				streamed = true
			}
			if err := emit(ctx, agentcore.MessageStartEvent{Message: e.Partial}); err != nil {
				return attemptResult{}, err
			}
		case provider.StreamTextEvent:
			if err := update(e.Partial, e); err != nil {
				return attemptResult{}, err
			}
		case provider.StreamThinkingEvent:
			if err := update(e.Partial, e); err != nil {
				return attemptResult{}, err
			}
		case provider.StreamToolCallEvent:
			if err := update(e.Partial, e); err != nil {
				return attemptResult{}, err
			}
		case provider.StreamDoneEvent:
			finalizeMessage(agentCtx, e.Message, &addedPartial)
			return attemptResult{Message: e.Message, Streamed: streamed}, nil
		case provider.StreamErrorEvent:
			finalizeMessage(agentCtx, e.Message, &addedPartial)
			return attemptResult{Message: e.Message, Failure: streamFailure(e), Streamed: streamed}, nil
		}
	}

	// 7. stream ended without done/error: fall back to the stream result.
	final, resErr := stream.Result(ctx)
	if resErr != nil {
		return attemptResult{Message: newErrorAssistantMessage(cfg, resErr), Failure: resErr, Streamed: streamed}, nil
	}
	finalizeMessage(agentCtx, final, &addedPartial)
	return attemptResult{Message: final, Streamed: streamed}, nil
}

// streamFailure extracts the cause of a terminal error event. Providers are
// expected to carry the structured error (Err); a provider that reports a
// failure with no cause still gets a non-nil error so the attempt is recognised
// as failed — it simply will not classify as transient.
func streamFailure(e provider.StreamErrorEvent) error {
	if e.Err != nil {
		return e.Err
	}
	reason := e.Message.ErrorMessage
	if reason == "" {
		reason = "provider reported a failure with no message"
	}
	return errors.New(reason)
}

// finalizeMessage replaces the placeholder partial with the final message, or
// appends it if the provider sent done/error without a prior start.
func finalizeMessage(agentCtx *agentcore.AgentContext, final agentcore.AssistantMessage, addedPartial *bool) {
	if *addedPartial {
		agentCtx.Messages[len(agentCtx.Messages)-1] = final
	} else {
		agentCtx.Messages = append(agentCtx.Messages, final)
		*addedPartial = true
	}
}

// newErrorAssistantMessage builds a terminal assistant message for an early
// failure that never produced a provider stream.
func newErrorAssistantMessage(cfg LoopConfig, err error) agentcore.AssistantMessage {
	return agentcore.AssistantMessage{
		RoleField:    agentcore.RoleAssistant,
		Model:        cfg.Model,
		Provider:     cfg.Provider,
		StopReason:   agentcore.StopReasonError,
		ErrorMessage: err.Error(),
	}
}

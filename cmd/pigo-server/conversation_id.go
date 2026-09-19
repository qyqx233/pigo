// The conversation id a custom provider can ask for: the session id, sent as
// conversation_id in the request body.
//
// CodeBuddy gateways (workbuddy2api) forward it upstream as X-Conversation-ID,
// and the upstream keeps a conversation on one prompt cache only when it has
// one: measured on hy3, a growing tool conversation hit 0% of its prefix
// without it and 87–89% with it. It is a per-provider switch because other
// OpenAI-compatible servers may reject a field they do not know.
//
// See spec/conversation-id.md.
package main

import (
	"context"

	"github.com/smallnest/pigo/internal/provider"
)

// conversationStream adds the session id to each call whose provider has the
// switch on. The switch is read per call, so turning it on or off applies from
// the next call of every session, loaded or not. It decorates the stream
// rather than the run config because summary (compaction) calls build their
// own request config, and they belong to the same conversation.
func (s *apiServer) conversationStream(inner provider.StreamFn, providerName, sessionID string) provider.StreamFn {
	if inner == nil || sessionID == "" {
		return inner
	}
	return func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		if custom, ok := s.settings.findCustomProvider(providerName); ok && custom.ConversationID {
			cfg.Extra = withExtraBody(cfg.Extra, "conversation_id", sessionID)
		}
		return inner(ctx, model, llm, cfg)
	}
}

// withExtraBody returns a copy of extra with one more body field; the caller's
// maps are left alone, since the run config shares them across calls.
func withExtraBody(extra map[string]any, key string, value any) map[string]any {
	out := make(map[string]any, len(extra)+1)
	for k, v := range extra {
		out[k] = v
	}
	body := map[string]any{}
	if existing, ok := extra[provider.ExtraBody].(map[string]any); ok {
		for k, v := range existing {
			body[k] = v
		}
	}
	body[key] = value
	out[provider.ExtraBody] = body
	return out
}

package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/provider"
)

// captureStream is a provider stream that records the config of each call.
func captureStream(seen *[]provider.StreamConfig) provider.StreamFn {
	return func(_ context.Context, _ string, _ provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		*seen = append(*seen, cfg)
		out := provider.NewAssistantMessageEventStream(0)
		out.Close()
		return out, nil
	}
}

func conversationIDOf(cfg provider.StreamConfig) any {
	body, _ := cfg.Extra[provider.ExtraBody].(map[string]any)
	return body["conversation_id"]
}

func TestConversationStream(t *testing.T) {
	server := newTestServer(t)
	for _, p := range []customProvider{
		{Name: "cbproxy", Protocol: "openai", BaseURL: "http://10.0.0.5/v1", ConversationID: true},
		{Name: "plain", Protocol: "openai", BaseURL: "http://10.0.0.6/v1"},
	} {
		if err := server.settings.putCustomProvider(p); err != nil {
			t.Fatal(err)
		}
	}
	var seen []provider.StreamConfig
	call := func(providerName string, extra map[string]any) {
		stream := server.conversationStream(captureStream(&seen), providerName, "sess-42")
		if _, err := stream(context.Background(), "m", provider.LlmContext{}, provider.StreamConfig{Extra: extra}); err != nil {
			t.Fatal(err)
		}
	}

	// On: the session id is added, and what was in Extra is kept — without
	// touching the caller's maps.
	shared := map[string]any{"max_tokens": 100, provider.ExtraBody: map[string]any{"user": "u1"}}
	call("cbproxy", shared)
	got := seen[len(seen)-1]
	if conversationIDOf(got) != "sess-42" || got.Extra["max_tokens"] != 100 || got.Extra[provider.ExtraBody].(map[string]any)["user"] != "u1" {
		t.Errorf("flagged call Extra = %v", got.Extra)
	}
	if _, touched := shared[provider.ExtraBody].(map[string]any)["conversation_id"]; touched {
		t.Error("the caller's Extra was modified")
	}

	// Off, and other providers: nothing added.
	call("plain", nil)
	call("openrouter", nil)
	for _, cfg := range seen[1:] {
		if conversationIDOf(cfg) != nil {
			t.Errorf("unflagged call got %v", cfg.Extra)
		}
	}

	// The switch is read per call: turning it off applies to the next call.
	if err := server.settings.putCustomProvider(customProvider{Name: "cbproxy", Protocol: "openai", BaseURL: "http://10.0.0.5/v1"}); err != nil {
		t.Fatal(err)
	}
	call("cbproxy", nil)
	if conversationIDOf(seen[len(seen)-1]) != nil {
		t.Error("still sent after the switch was turned off")
	}
}

func TestConversationIDNeedsOpenAIProtocol(t *testing.T) {
	_, err := customProvider{Name: "cb", Protocol: "anthropic", BaseURL: "http://x/v1", ConversationID: true}.validate()
	if err == nil || !strings.Contains(err.Error(), "openai") {
		t.Errorf("err = %v", err)
	}
	if _, err := (customProvider{Name: "cb", Protocol: "openai", BaseURL: "http://x/v1", ConversationID: true}).validate(); err != nil {
		t.Errorf("openai with the switch: %v", err)
	}
}

// TestConversationIDSurvivesEditAndRename checks the switch is part of the
// definition: the edit endpoint stores it, and a rename carries it.
func TestConversationIDSurvivesEditAndRename(t *testing.T) {
	server := newTestServer(t)
	if err := server.settings.putCustomProvider(customProvider{Name: "cb", Protocol: "openai", BaseURL: "http://x/v1"}); err != nil {
		t.Fatal(err)
	}
	if response := patchProvider(t, server, "cb", customProvider{Name: "cb2", Protocol: "openai", BaseURL: "http://x/v1", ConversationID: true}); response.Code != http.StatusOK {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
	got, ok := server.settings.findCustomProvider("cb2")
	if !ok || !got.ConversationID {
		t.Errorf("after rename = %+v", got)
	}
}

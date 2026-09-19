package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/provider"
)

// stubTool is a tool with a name and a description, for request shaping.
type stubTool struct {
	agentcore.AgentTool
	name, description string
}

func (t stubTool) Name() string        { return t.name }
func (t stubTool) Description() string { return t.description }

// namingServer is a server with the full test config loaded.
func namingServer(t *testing.T) *apiServer {
	t.Helper()
	x, err := parseToolsConfig([]byte(fullToolsConfig), testEnv(map[string]string{"SEARCH_TOKEN": "x"}))
	if err != nil {
		t.Fatal(err)
	}
	return &apiServer{exts: x}
}

// replyStream is a provider reply that calls the tool named wire.
func replyStream(wire string) *provider.AssistantMessageEventStream {
	msg := agentcore.AssistantMessage{
		RoleField: agentcore.RoleAssistant,
		Content: agentcore.ContentList{
			agentcore.NewTextContent("running it"),
			agentcore.ToolCallContent{Type: "toolCall", ID: "c1", Name: wire, Arguments: json.RawMessage(`{"command":"ls"}`)},
		},
		StopReason: "toolUse",
	}
	out := provider.NewAssistantMessageEventStream(4)
	go func() {
		_ = out.Emit(context.Background(), provider.StreamToolCallEvent{Partial: msg})
		_ = out.Emit(context.Background(), provider.StreamDoneEvent{Message: msg})
		out.Close()
	}()
	return out
}

func toolCallNames(m agentcore.AssistantMessage) []string {
	var out []string
	for _, c := range m.Content {
		if call, ok := c.(agentcore.ToolCallContent); ok {
			out = append(out, call.Name)
		}
	}
	return out
}

// TestNameStreamRenamesBothWays checks a claude-profile call: the provider
// sees the profile's names everywhere, and the reply comes back canonical.
func TestNameStreamRenamesBothWays(t *testing.T) {
	s := namingServer(t)
	var seen provider.LlmContext
	inner := func(_ context.Context, _ string, llm provider.LlmContext, _ provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		seen = llm
		return replyStream("Bash"), nil
	}
	history := agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("go")}},
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{
			agentcore.ToolCallContent{Type: "toolCall", ID: "c0", Name: "read", Arguments: json.RawMessage(`{}`)},
		}},
		agentcore.ToolResultMessage{RoleField: agentcore.RoleToolResult, ToolCallID: "c0", ToolName: "read"},
	}
	llm := provider.LlmContext{
		SystemPrompt: "Use the todo tool to plan. Use the read tool to load a skill; run `bash` or `grep`. Read the file.",
		Messages:     history,
		Tools: []agentcore.AgentTool{
			stubTool{name: "bash", description: "Run a command. Prefer the read tool for files."},
			stubTool{name: "websearch", description: "original"},
			stubTool{name: "grep", description: "Search."},
		},
	}

	stream, err := s.nameStream(inner, "openrouter")(context.Background(), "anthropic/claude-sonnet-5", llm, provider.StreamConfig{})
	if err != nil {
		t.Fatal(err)
	}

	// Outgoing.
	if got := seen.SystemPrompt; got != "Use the todo tool to plan. Use the Read tool to load a skill; run `Bash` or `grep`. Read the file." {
		t.Errorf("prompt = %q", got)
	}
	want := map[string]string{"Bash": "Run a command. Prefer the Read tool for files.", "WebSearch": "Search the web.", "grep": "Search."}
	for _, tool := range seen.Tools {
		if d, ok := want[tool.Name()]; !ok || d != tool.Description() {
			t.Errorf("tool %q: %q", tool.Name(), tool.Description())
		}
	}
	if got := toolCallNames(seen.Messages[1].(agentcore.AssistantMessage)); got[0] != "Read" {
		t.Errorf("history tool call = %v", got)
	}
	if got := seen.Messages[2].(agentcore.ToolResultMessage).ToolName; got != "Read" {
		t.Errorf("history tool result = %q", got)
	}
	// The caller's context is untouched.
	if llm.Tools[0].Name() != "bash" || toolCallNames(history[1].(agentcore.AssistantMessage))[0] != "read" || history[2].(agentcore.ToolResultMessage).ToolName != "read" {
		t.Error("the caller's context was modified")
	}

	// Incoming: every event and the result carry the canonical name.
	for ev := range stream.Events() {
		var msg agentcore.AssistantMessage
		switch e := ev.(type) {
		case provider.StreamToolCallEvent:
			msg = e.Partial
		case provider.StreamDoneEvent:
			msg = e.Message
		}
		if got := toolCallNames(msg); len(got) > 0 && got[0] != "bash" {
			t.Errorf("%T tool call = %v", ev, got)
		}
	}
	res, err := stream.Result(context.Background())
	if err != nil || toolCallNames(res)[0] != "bash" {
		t.Errorf("result = %v %v", toolCallNames(res), err)
	}
}

// TestNameStreamPassesUnmatched checks that a model no rule matches is sent
// the canonical names, through the same stream.
func TestNameStreamPassesUnmatched(t *testing.T) {
	s := namingServer(t)
	var seen provider.LlmContext
	direct := replyStream("bash")
	inner := func(_ context.Context, _ string, llm provider.LlmContext, _ provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		seen = llm
		return direct, nil
	}
	llm := provider.LlmContext{SystemPrompt: "the bash tool", Tools: []agentcore.AgentTool{stubTool{name: "bash"}}}
	stream, _ := s.nameStream(inner, "deepseek")(context.Background(), "deepseek-v4-flash", llm, provider.StreamConfig{})
	if stream != direct || seen.SystemPrompt != "the bash tool" || seen.Tools[0].Name() != "bash" {
		t.Errorf("unmatched call was changed: %+v", seen)
	}
	// With no rules at all the stream is not wrapped.
	none := &apiServer{exts: &toolExtensions{}}
	if got := none.nameStream(inner, "x"); got == nil {
		t.Error("nameStream dropped the stream")
	}
}

// TestNameStreamSwitchesWithModel checks one session's two models get their
// own names: the profile is chosen per call.
func TestNameStreamSwitchesWithModel(t *testing.T) {
	s := namingServer(t)
	var names []string
	inner := func(_ context.Context, _ string, llm provider.LlmContext, _ provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		names = append(names, llm.Tools[0].Name())
		return replyStream("x"), nil
	}
	stream := s.nameStream(inner, "codebuddy-proxy")
	llm := provider.LlmContext{Tools: []agentcore.AgentTool{stubTool{name: "websearch"}}}
	for _, model := range []string{"claude-opus-5", "global:hy3", "claude-opus-5"} {
		out, _ := stream(context.Background(), model, llm, provider.StreamConfig{})
		for range out.Events() {
		}
	}
	if len(names) != 3 || names[0] != "WebSearch" || names[1] != "web_search" || names[2] != "WebSearch" {
		t.Errorf("names = %v", names)
	}
}

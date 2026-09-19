// Per-model tool naming: models have preferences in what their tools are
// called (Bash / bash, WebSearch / web_search), so a naming profile renames
// the tools for the calls it applies to. The renaming happens only on the way
// to the model and back — the tool declarations, the tool names in the
// history, the system prompt, and the tool calls in the reply — so everything
// else (the transcript, the activity log, billing, -tools) only ever sees the
// canonical names, and switching a session's model switches the names with
// it.
//
// See spec/tool-extensions.md, section 7.
package main

import (
	"context"
	"regexp"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/provider"
)

// nameStream decorates a provider stream with the naming profile that applies
// to each call, chosen by the call's provider and model.
func (s *apiServer) nameStream(inner provider.StreamFn, providerName string) provider.StreamFn {
	if inner == nil || s.exts == nil || len(s.exts.rules) == 0 {
		return inner
	}
	return func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		profile := s.exts.profileFor(providerName, model)
		if profile == nil {
			return inner(ctx, model, llm, cfg)
		}
		in, err := inner(ctx, model, profile.outgoing(llm), cfg)
		if err != nil || in == nil {
			return in, err
		}
		out := provider.NewAssistantMessageEventStream(0)
		go profile.relay(ctx, in, out)
		return out, nil
	}
}

// outgoing renames a request: a copy, the caller's context is left alone.
func (p *namingProfile) outgoing(llm provider.LlmContext) provider.LlmContext {
	out := provider.LlmContext{SystemPrompt: p.rewriteMentions(llm.SystemPrompt)}
	for _, t := range llm.Tools {
		out.Tools = append(out.Tools, p.renamedTool(t))
	}
	out.Messages = make(agentcore.MessageList, len(llm.Messages))
	for i, m := range llm.Messages {
		out.Messages[i] = p.renameMessage(m, p.wire)
	}
	return out
}

// renamedTool presents a tool under the profile's name and description.
func (p *namingProfile) renamedTool(t agentcore.AgentTool) agentcore.AgentTool {
	entry := p.forward[t.Name()]
	description := entry.Description
	if description == "" {
		description = p.rewriteMentions(t.Description())
	}
	return &wireTool{AgentTool: t, name: p.wire(t.Name()), description: description}
}

type wireTool struct {
	agentcore.AgentTool
	name        string
	description string
}

func (t *wireTool) Name() string        { return t.name }
func (t *wireTool) Description() string { return t.description }

// renameMessage maps the tool names in one message; messages without any are
// returned as they are.
func (p *namingProfile) renameMessage(m agentcore.Message, rename func(string) string) agentcore.Message {
	switch msg := m.(type) {
	case agentcore.AssistantMessage:
		return p.renameAssistant(msg, rename)
	case *agentcore.AssistantMessage:
		renamed := p.renameAssistant(*msg, rename)
		return &renamed
	case agentcore.ToolResultMessage:
		msg.ToolName = rename(msg.ToolName)
		return msg
	case *agentcore.ToolResultMessage:
		copied := *msg
		copied.ToolName = rename(copied.ToolName)
		return &copied
	}
	return m
}

// renameAssistant maps an assistant message's tool calls. The content list is
// copied before a change: a provider's streaming partial shares it with the
// provider's own buffer.
func (p *namingProfile) renameAssistant(msg agentcore.AssistantMessage, rename func(string) string) agentcore.AssistantMessage {
	var content agentcore.ContentList
	for i, c := range msg.Content {
		call, ok := c.(agentcore.ToolCallContent)
		if !ok {
			continue
		}
		name := rename(call.Name)
		if name == call.Name {
			continue
		}
		if content == nil {
			content = append(agentcore.ContentList(nil), msg.Content...)
		}
		call.Name = name
		content[i] = call
	}
	if content != nil {
		msg.Content = content
	}
	return msg
}

// relay forwards the reply with its tool calls renamed back to the canonical
// names. Like the meter's relay it drains the inner stream even after the
// consumer stops reading.
func (p *namingProfile) relay(ctx context.Context, in, out *provider.AssistantMessageEventStream) {
	back := func(m agentcore.AssistantMessage) agentcore.AssistantMessage {
		return p.renameAssistant(m, p.canonical)
	}
	forward := true
	for ev := range in.Events() {
		switch e := ev.(type) {
		case provider.StreamStartEvent:
			e.Partial = back(e.Partial)
			ev = e
		case provider.StreamTextEvent:
			e.Partial = back(e.Partial)
			ev = e
		case provider.StreamThinkingEvent:
			e.Partial = back(e.Partial)
			ev = e
		case provider.StreamToolCallEvent:
			e.Partial = back(e.Partial)
			ev = e
		case provider.StreamDoneEvent:
			e.Message = back(e.Message)
			ev = e
		case provider.StreamErrorEvent:
			e.Message = back(e.Message)
			ev = e
		}
		if forward && out.Emit(ctx, ev) != nil {
			forward = false
		}
	}
	if res, err := in.Result(context.Background()); err != nil {
		out.SetError(err)
	} else {
		out.SetResult(back(res))
	}
	out.Close()
}

// Mentions of a tool in prose. Only these two shapes are rewritten — "read" or
// "find" as a bare word is far too common to replace.
var (
	theToolMention  = regexp.MustCompile(`\bthe ([A-Za-z0-9_-]+) tool\b`)
	backtickMention = regexp.MustCompile("`([A-Za-z0-9_-]+)`")
)

// rewriteMentions renames "the <name> tool" and `<name>` in text. One pass per
// shape, so a renamed name is never renamed again.
func (p *namingProfile) rewriteMentions(text string) string {
	if text == "" || len(p.reverse) == 0 {
		return text
	}
	text = theToolMention.ReplaceAllStringFunc(text, func(m string) string {
		name := theToolMention.FindStringSubmatch(m)[1]
		return "the " + p.wire(name) + " tool"
	})
	return backtickMention.ReplaceAllStringFunc(text, func(m string) string {
		return "`" + p.wire(m[1:len(m)-1]) + "`"
	})
}

// profileName is the naming profile a session's current model gets, "" for
// none — for display.
func (s *apiServer) profileName(providerName, model string) string {
	if p := s.exts.profileFor(providerName, model); p != nil {
		return p.name
	}
	return ""
}

// wireNames maps canonical tool names to what a profile calls them, for display.
func (p *namingProfile) wireNames() map[string]string {
	out := map[string]string{}
	for canonical := range p.forward {
		if wire := p.wire(canonical); wire != canonical {
			out[canonical] = wire
		}
	}
	return out
}

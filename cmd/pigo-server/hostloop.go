package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/agenttool"
	"github.com/smallnest/pigo/internal/cli/run"
	"github.com/smallnest/pigo/internal/provider"
	"github.com/smallnest/pigo/internal/runtime"
	"github.com/smallnest/pigo/internal/session"
)

const transcriptID = "chat"

func (s *apiServer) sandboxSpec(managed *managedSession) RunSpec {
	return RunSpec{
		Workspace:  managed.paths.Workspace,
		Home:       managed.paths.Home,
		NoSkills:   !s.config.skills,
		SkillsHost: skillsDir(),
		// Env stays empty: provider keys never enter the sandbox.
	}
}

func (s *apiServer) ensureHostLoop(managed *managedSession) error {
	if managed.agentCtx != nil && managed.runCfg.Stream != nil {
		return nil
	}
	ws := managed.paths.Workspace
	tools := s.hostTools(managed)
	prompt, err := runtime.BuildSystemPrompt(runtime.PromptConfig{
		WorkingDir:        ws,
		Root:              ws,
		ReadToolAvailable: hasHostRead(tools),
	})
	if err != nil {
		return err
	}
	prov, providerName, err := provider.ResolveProvider(managed.meta.Model, "", "", s.config.provider, os.Getenv)
	if err != nil {
		return err
	}
	managed.meta.Provider = providerName
	creds := provider.NewCredentialStore(nil)
	thinking := agentcore.ThinkingLevel(managed.meta.Thinking)
	if thinking == "" {
		thinking = agentcore.ThinkingMedium
	}
	reg := run.ToolRegistry(tools)
	managed.runCfg = run.NewConfig(managed.meta.Model, providerName, thinking, prov, creds, reg, run.TodoReminders(tools))
	msgs := s.loadTranscript(managed)
	managed.agentCtx = &agentcore.AgentContext{
		SystemPrompt: prompt,
		Messages:     msgs,
		Tools:        tools,
	}
	return nil
}

func (s *apiServer) hostTools(managed *managedSession) []agentcore.AgentTool {
	if s.noTools {
		return nil
	}
	ws := managed.paths.Workspace
	snap := agenttool.NewFileSnapshotRecorder()
	var extra []string
	if s.config.skills {
		if dir := skillsDir(); dir != "" {
			extra = []string{dir}
		}
	}
	tools := []agentcore.AgentTool{
		&agenttool.ReadTool{Root: ws, ExtraRoots: extra},
		&agenttool.WriteTool{Root: ws, ExtraRoots: extra, Snap: snap},
		&agenttool.EditTool{Root: ws, ExtraRoots: extra, Snap: snap},
		&agenttool.GrepTool{Root: ws},
		&agenttool.FindTool{Root: ws},
		&sandboxBashTool{server: s, session: managed},
		&agenttool.TodoTool{Store: agenttool.NewTodoStore()},
		&agenttool.WebFetchTool{},
		&agenttool.WebSearchTool{},
	}
	if !toolsAreAll(s.config.tools) && len(s.toolNames) > 0 {
		policy := run.NewToolPolicy(s.toolNames, nil)
		tools = run.ApplyToolPolicy(tools, policy)
	}
	return tools
}

func hasHostRead(tools []agentcore.AgentTool) bool {
	for _, t := range tools {
		if t.Name() == "read" {
			return true
		}
	}
	return false
}

func (s *apiServer) runHostLoop(ctx context.Context, managed *managedSession, prompt string, emit func(streamEvent)) (string, error) {
	if err := s.ensureHostLoop(managed); err != nil {
		return "", err
	}
	managed.agentCtx.Messages = append(managed.agentCtx.Messages, agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   agentcore.ContentList{agentcore.NewTextContent(prompt)},
	})
	stream := runtime.StartRun(ctx, managed.agentCtx, managed.runCfg)
	final, err := runtime.DrainStream(ctx, stream, runtime.StreamHandler{
		OnText: func(delta string) {
			if emit != nil {
				emit(streamEvent{Type: "delta", Text: delta})
			}
		},
		OnEvent: func(ev agentcore.AgentEvent) {
			if emit == nil {
				return
			}
			switch e := ev.(type) {
			case agentcore.ToolExecutionStartEvent:
				emit(streamEvent{Type: "tool", Tool: e.ToolName, Phase: "start", ID: e.ToolCallID})
			case agentcore.ToolExecutionEndEvent:
				emit(streamEvent{Type: "tool", Tool: e.ToolName, Phase: "end", ID: e.ToolCallID, IsError: e.IsError})
			}
		},
	})
	s.saveTranscript(managed)
	if err != nil {
		return "", err
	}
	if final == nil {
		return "", nil
	}
	text := agentcore.ContentToText(final.Content)
	switch final.StopReason {
	case agentcore.StopReasonError:
		reason := final.ErrorMessage
		if reason == "" {
			reason = "error"
		}
		return text, fmt.Errorf("%s", reason)
	case agentcore.StopReasonAborted:
		if ctx.Err() != nil {
			return text, ctx.Err()
		}
		return text, fmt.Errorf("aborted")
	default:
		return text, nil
	}
}

func (s *apiServer) loadTranscript(managed *managedSession) agentcore.MessageList {
	store, err := session.NewStore(filepath.Join(managed.paths.Root, "transcript"))
	if err != nil {
		return nil
	}
	_, msgs, err := store.Load(transcriptID)
	if err != nil {
		return nil
	}
	return msgs
}

func (s *apiServer) saveTranscript(managed *managedSession) {
	if managed.agentCtx == nil {
		return
	}
	store, err := session.NewStore(filepath.Join(managed.paths.Root, "transcript"))
	if err != nil {
		return
	}
	_ = store.Save(session.SessionHeader{
		ID:        transcriptID,
		CreatedAt: managed.meta.CreatedAt,
		UpdatedAt: time.Now().UTC(),
		Model:     managed.meta.Model,
		Provider:  managed.meta.Provider,
		Cwd:       managed.paths.Workspace,
	}, managed.agentCtx.Messages)
}

func (s *apiServer) applyHostConfig(managed *managedSession) error {
	if managed.agentCtx == nil {
		return nil
	}
	prov, providerName, err := provider.ResolveProvider(managed.meta.Model, "", "", s.config.provider, os.Getenv)
	if err != nil {
		return err
	}
	managed.meta.Provider = providerName
	creds := provider.NewCredentialStore(nil)
	thinking := agentcore.ThinkingLevel(managed.meta.Thinking)
	if thinking == "" {
		thinking = agentcore.ThinkingMedium
	}
	managed.runCfg.Model = managed.meta.Model
	managed.runCfg.Provider = providerName
	managed.runCfg.ThinkingLevel = thinking
	managed.runCfg.Stream = provider.StreamFnFromProvider(prov)
	managed.runCfg.GetAPIKey = creds.GetAPIKey
	return nil
}

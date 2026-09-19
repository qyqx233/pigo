package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	prompt = virtualizeWorkspacePrompt(prompt, ws)
	prov, providerName, err := s.resolveProvider(managed.meta.Model, managed.meta.Provider)
	if err != nil {
		return err
	}
	managed.meta.Provider = providerName
	creds := s.credentialStoreFor(managed, providerName)
	thinking := agentcore.ThinkingLevel(managed.meta.Thinking)
	if thinking == "" {
		thinking = agentcore.ThinkingMedium
	}
	reg := run.ToolRegistry(tools)
	managed.runCfg = run.NewConfig(managed.meta.Model, providerName, thinking, prov, creds, reg, run.TodoReminders(tools), nil)
	s.meterStreams(managed, prov, providerName)
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
		withVirtualWorkspace(&agenttool.ReadTool{Root: ws, ExtraRoots: extra}),
		withVirtualWorkspace(&agenttool.WriteTool{Root: ws, ExtraRoots: extra, Snap: snap}),
		withVirtualWorkspace(&agenttool.EditTool{Root: ws, ExtraRoots: extra, Snap: snap}),
		withVirtualWorkspace(&agenttool.GrepTool{Root: ws}),
		withVirtualWorkspace(&agenttool.FindTool{Root: ws}),
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
	// Every model call the turn makes is metered under this turn, and the turn
	// does not report itself finished until each call's cost is recorded — so
	// the client sees the last "usage" event before "done".
	turn := s.newTurn(managed, emit)
	ctx = withTurn(ctx, turn)
	defer turn.wait(10 * time.Second)
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
			case agentcore.RetryEvent:
				// Transient status, not content and not a tool: the request is
				// being re-issued after a rate limit / overload, and the wait is
				// long enough that a client showing nothing looks hung.
				emit(streamEvent{Type: "notice", Notice: "retry",
					Text: fmt.Sprintf("请求失败（%s），%s 后重试（%d/%d）",
						e.Reason, e.Delay.Round(time.Second), e.Attempt, e.MaxRetries)})
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
		Cwd:       virtualWorkspaceRoot,
	}, managed.agentCtx.Messages)
}

func (s *apiServer) applyHostConfig(managed *managedSession) error {
	if managed.agentCtx == nil {
		return nil
	}
	prov, providerName, err := s.resolveProvider(managed.meta.Model, managed.meta.Provider)
	if err != nil {
		return err
	}
	managed.meta.Provider = providerName
	creds := s.credentialStoreFor(managed, providerName)
	thinking := agentcore.ThinkingLevel(managed.meta.Thinking)
	if thinking == "" {
		thinking = agentcore.ThinkingMedium
	}
	managed.runCfg.Model = managed.meta.Model
	managed.runCfg.Provider = providerName
	managed.runCfg.ThinkingLevel = thinking
	s.meterStreams(managed, prov, providerName)
	managed.runCfg.GetAPIKey = creds.GetAPIKey
	return nil
}

// meterStreams points the session's model calls at the provider through the
// billing meter: the conversation's calls, and the summary calls that compact a
// long context (which otherwise fall back to the same unmetered stream).
func (s *apiServer) meterStreams(managed *managedSession, prov provider.Provider, providerName string) {
	stream := provider.StreamFnFromProvider(prov)
	managed.runCfg.Stream = s.meter.wrap(stream, providerName, "chat")
	managed.runCfg.SummaryStream = s.meter.wrap(stream, providerName, "compaction")
}

// newTurn describes one user turn for the billing meter.
func (s *apiServer) newTurn(managed *managedSession, emit func(streamEvent)) *turnInfo {
	userID := managed.meta.UserID
	username := ""
	if s.auth != nil && userID != "" {
		if user, ok := s.auth.findUser(userID); ok {
			username = user.Username
		}
	}
	return &turnInfo{
		userID:    userID,
		username:  username,
		sessionID: managed.meta.ID,
		turnID:    newLedgerID(),
		keySource: s.keySource(userID, managed.meta.Provider),
		emit:      emit,
	}
}

// keySource reports where a call's key comes from, which decides who pays.
func (s *apiServer) keySource(userID, providerName string) string {
	envKey := false
	if spec, ok := provider.LookupProviderSpec(providerName); ok {
		envKey = envHasKey(spec)
	}
	return s.credentialSource(userID, providerName, envKey)
}

// credentialStoreFor builds the provider credential store for one session,
// applying the precedence the deployment promises: the session owner's own key,
// then the administrator's shared pool, then nothing — which leaves the store
// to fall back to the process environment exactly as it did before this
// feature existed.
//
// The override is resolved per session rather than once per process because the
// owner and the administrator can both change a key while the server runs;
// applyHostConfig re-runs this on the next request, so an idle session picks up
// a new key without being recreated.
func (s *apiServer) credentialStoreFor(managed *managedSession, providerName string) *provider.CredentialStore {
	creds := provider.NewCredentialStore(nil)
	if s.credentials == nil {
		return creds
	}
	// SetOverride ignores an empty key, so the environment fallback survives.
	creds.SetOverride(providerName, s.credentials.resolve(
		managed.meta.UserID,
		providerName,
		s.settings.get().AllowUserKeys,
	))
	return creds
}

// providerResolver maps a model and provider name onto a wire driver. It is
// threaded through the free functions that need it (slash commands, model
// validation) rather than reaching for the server, so they stay testable and so
// the compiler finds every place that must honour custom endpoints.
type providerResolver func(model, providerName string) (provider.Provider, string, error)

// resolveProviderWith is the shared body: it consults the deployment's custom
// endpoints before falling back to the registry.
//
// A name the administrator defined as a custom endpoint is resolved by protocol
// and base URL rather than through the registry, and — this is the part that
// matters — the custom name is kept. provider.ResolveProvider's protocol branch
// reports the generic driver identity ("openai" / "anthropic"); letting that
// name through would file the endpoint's credentials under the built-in
// provider's key and hand a self-hosted gateway the real OpenAI key.
//
// Everything else resolves exactly as before.
func resolveProviderWith(settings *settingsStore, model, providerName string) (provider.Provider, string, error) {
	if custom, ok := settings.findCustomProvider(providerName); ok {
		prov, _, err := provider.ResolveProvider(model, custom.BaseURL, custom.Protocol, "", os.Getenv)
		if err != nil {
			return nil, "", fmt.Errorf("custom provider %q: %w", custom.Name, err)
		}
		return prov, custom.Name, nil
	}
	return provider.ResolveProvider(model, "", "", providerName, os.Getenv)
}

// resolveModel resolves a model a user picked, honouring the catalog: when no
// provider is named, an entry the user can see (their own, or the shared one)
// names it. Without this, "/model bonai2" falls through to the name heuristics,
// which know nothing about this deployment's endpoints and land on openrouter —
// and the session then sends bonai2 to the wrong server.
func (s *apiServer) resolveModel(userID, model, providerName string) (provider.Provider, string, error) {
	if strings.TrimSpace(providerName) == "" {
		if entry, ok := s.customModels.find(userID, strings.TrimSpace(model)); ok {
			providerName = entry.Provider
		}
	}
	return s.resolveProvider(model, providerName)
}

// resolverFor binds resolveModel to one user, in the shape the slash-command
// helpers take.
func (s *apiServer) resolverFor(userID string) providerResolver {
	return func(model, providerName string) (provider.Provider, string, error) {
		return s.resolveModel(userID, model, providerName)
	}
}

// resolveProvider is the server's own resolver.
func (s *apiServer) resolveProvider(model, providerName string) (provider.Provider, string, error) {
	return resolveProviderWith(s.settings, model, providerName)
}

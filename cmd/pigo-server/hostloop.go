package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/agenttool"
	"github.com/smallnest/pigo/internal/cli/run"
	"github.com/smallnest/pigo/internal/provider"
	"github.com/smallnest/pigo/internal/runtime"
)

func (s *apiServer) sandboxSpec(managed *managedSession) RunSpec {
	return RunSpec{
		Workspace:  managed.paths.Workspace,
		Home:       managed.paths.Home,
		NoSkills:   !s.config.skills,
		SkillsHost: skillsDir(),
		// Only what an administrator configured (a proxy, a tool's endpoint):
		// the server's own environment, and with it the provider keys, never
		// enters the sandbox.
		Env: s.settings.sandboxEnvPairs(),
	}
}

func (s *apiServer) ensureHostLoop(managed *managedSession) error {
	if managed.agentCtx != nil && managed.runCfg.Stream != nil {
		return nil
	}
	tools, err := s.hostTools(managed)
	if err != nil {
		return err
	}
	// The provider is resolved first: the prompt states the model's knowledge
	// cutoff, which is configured per provider + model.
	prov, providerName, err := s.resolveProvider(managed.meta.Model, managed.meta.Provider)
	if err != nil {
		return err
	}
	managed.meta.Provider = providerName
	cutoff := s.knowledgeCutoff(providerName, managed.meta.Model)
	prompt, err := s.sessionPrompt(managed, tools, cutoff)
	if err != nil {
		return err
	}
	thinking := agentcore.ThinkingLevel(managed.meta.Thinking)
	if thinking == "" {
		thinking = agentcore.ThinkingMedium
	}
	reg := run.ToolRegistry(tools)
	managed.runCfg = run.NewConfig(managed.meta.Model, providerName, thinking, prov, provider.NewCredentialStore(nil), reg, run.TodoReminders(tools), nil)
	// NewConfig's store falls back to the process environment; the server's
	// keys come from its own store only.
	managed.runCfg.GetAPIKey = s.apiKeyFunc(managed.meta.UserID)
	s.meterStreams(managed, prov, providerName)
	msgs := s.loadTranscript(managed)
	managed.agentCtx = &agentcore.AgentContext{
		SystemPrompt: prompt,
		Messages:     msgs,
		Tools:        tools,
	}
	managed.promptKey, managed.promptCutoff = priceKey(providerName, managed.meta.Model), cutoff
	return nil
}

// sessionPrompt assembles a session's system prompt: the base instruction and
// environment block, the knowledge-cutoff note, the workspace virtualization,
// the sandbox toolchains and the scene. It reads the workspace's AGENTS.md, so
// a session builds it once; a later change of model swaps the cutoff note in
// place instead of calling this again.
func (s *apiServer) sessionPrompt(managed *managedSession, tools []agentcore.AgentTool, cutoff string) (string, error) {
	ws := managed.paths.Workspace
	// One timestamp for both the environment block and the anchor the cutoff
	// note is inserted after.
	now := time.Now()
	prompt, err := runtime.BuildSystemPrompt(runtime.PromptConfig{
		WorkingDir:        ws,
		Root:              ws,
		ReadToolAvailable: hasHostRead(tools),
		Now:               func() time.Time { return now },
	})
	if err != nil {
		return "", err
	}
	prompt, _ = withKnowledgeCutoff(prompt, now, cutoff)
	prompt = virtualizeWorkspacePrompt(prompt, ws)
	if hasHostBash(tools) {
		prompt += toolPromptNote(s.sandbox.Tools)
	}
	if sc := managed.meta.Scene; sc != nil {
		prompt += sceneSystemPrompt(sc)
	}
	return prompt, nil
}

// hostTools is a session's tool set: the built-ins, then the configured
// extensions (replacing or adding), then the -tools selection.
func (s *apiServer) hostTools(managed *managedSession) ([]agentcore.AgentTool, error) {
	if s.noTools {
		return nil, nil
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
	tools, err := s.applyExtensions(managed, tools)
	if err != nil {
		return nil, err
	}
	// The task tool, when the deployment allows it. A scene session does not
	// get one: a scene is a controlled setup, and a sub-agent does not inherit
	// its prompt. Sub-agents build their own set and drop this (subagent.go).
	if !s.settings.subagent().Disabled && managed.meta.Scene == nil {
		tools = append(tools, &taskTool{server: s, session: managed})
	}
	if !toolsAreAll(s.config.tools) && len(s.toolNames) > 0 {
		policy := run.NewToolPolicy(s.toolNames, nil)
		tools = run.ApplyToolPolicy(tools, policy)
	}
	// A scene session has the scene's tools: the listed ones of the set, and
	// the scene tools it names.
	if sc := managed.meta.Scene; sc != nil && len(sc.Tools) > 0 {
		tools = keepTools(tools, sc.Tools)
		extra, err := s.sceneTools(managed, sc.Tools)
		if err != nil {
			return nil, err
		}
		tools = append(tools, extra...)
	}
	return tools, nil
}

func hasHostBash(tools []agentcore.AgentTool) bool {
	for _, t := range tools {
		if t.Name() == "bash" {
			return true
		}
	}
	return false
}

func hasHostRead(tools []agentcore.AgentTool) bool {
	for _, t := range tools {
		if t.Name() == "read" {
			return true
		}
	}
	return false
}

// runHostLoop runs one turn of the agent. It holds the session lock only to set
// up: from then on the turn owns the agent context (no other path touches it
// while a turn runs) and works on its own copy of the run configuration, so a
// settings change made meanwhile applies to the next turn.
//
// The transcript is saved when the user's message is added and again after
// every step (see turnRun.installHooks), so a restart loses at most the step
// in progress.
func (s *apiServer) runHostLoop(ctx context.Context, managed *managedSession, prompt string, emit func(streamEvent)) (string, error) {
	managed.mu.Lock()
	if err := s.ensureHostLoop(managed); err != nil {
		managed.mu.Unlock()
		return "", err
	}
	if err := s.errNoProviderKey(managed.meta.UserID, managed.meta.Provider); err != nil {
		managed.mu.Unlock()
		return "", err
	}
	agentCtx := managed.agentCtx
	agentCtx.Messages = append(agentCtx.Messages, agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   agentcore.ContentList{agentcore.NewTextContent(prompt)},
	})
	cfg := managed.runCfg
	// A scene command brings its scene tools for this turn only.
	if names := sceneToolsFrom(ctx); len(names) > 0 {
		extra, err := s.sceneTools(managed, names)
		if err != nil {
			agentCtx.Messages = agentCtx.Messages[:len(agentCtx.Messages)-1]
			managed.mu.Unlock()
			return "", err
		}
		if extra = missingTools(agentCtx.Tools, extra); len(extra) > 0 {
			base := agentCtx.Tools
			turnTools := append(append([]agentcore.AgentTool(nil), base...), extra...)
			agentCtx.Tools = turnTools
			cfg.Batch.ToolExecutorConfig.Registry = run.ToolRegistry(turnTools)
			defer func() {
				managed.mu.Lock()
				agentCtx.Tools = base
				managed.mu.Unlock()
			}()
		}
	}
	// Context compaction, with the window and threshold that apply now: an
	// administrator's change takes effect from the next turn.
	params := s.contextParams(managed.meta.Provider, managed.meta.Model)
	cfg.ContextWindow = params.Window
	cfg.Compaction = params.settings()
	// Every model call the turn makes is metered under this turn, and the turn
	// does not report itself finished until each call's cost is recorded — so
	// the client sees the last "usage" event before "done".
	turn := s.newTurn(managed, emit)
	managed.mu.Unlock()
	if runFrom(ctx) != nil {
		// The turn's usage is committed with it when it ends.
		turn.hold()
	}

	run := runFrom(ctx)
	if run != nil {
		run.installHooks(&cfg, len(agentCtx.Messages), func(ac *agentcore.AgentContext) {
			if s.checkpoint(managed, ac.Messages) {
				run.transcriptRewritten()
			}
		})
	}
	rewrote := s.checkpoint(managed, agentCtx.Messages)
	if run != nil {
		// The user message is now the file's last entry. The file only grows,
		// so the position holds through a compaction; a rewrite moves it.
		if rewrote {
			run.transcriptRewritten()
		} else if n := s.transcriptLen(managed); n > 0 {
			run.setTranscriptStart(n - 1)
		}
	}

	// started times each tool call, for the activity log. OnEvent runs on
	// one goroutine, so it needs no lock.
	started := map[string]time.Time{}
	// compacting is the automatic compaction under way. The loop reports its
	// start, but when there is nothing old enough to summarize it returns
	// without an end; the next event, or the run's end, closes it as skipped.
	var compacting *compactionReport
	closeSkipped := func() {
		if compacting != nil && emit != nil {
			emit(streamEvent{Type: "compaction", Phase: "end", Compaction: &compactionReport{Reason: compacting.Reason, Before: compacting.Before, After: compacting.Before}})
		}
		compacting = nil
	}
	defer closeSkipped()
	ctx = withTurn(ctx, turn)
	defer func() {
		turn.wait(10 * time.Second)
		if run != nil {
			run.addLedger(turn.release())
		}
	}()
	stream := runtime.StartRun(ctx, agentCtx, cfg)
	final, err := runtime.DrainStream(ctx, stream, runtime.StreamHandler{
		OnText: func(delta string) {
			if emit != nil {
				emit(streamEvent{Type: "delta", Text: delta})
			}
		},
		OnEvent: func(ev agentcore.AgentEvent) {
			if run != nil {
				run.observe(ev)
			}
			if _, ends := ev.(agentcore.CompactionEvent); !ends {
				closeSkipped()
			}
			if emit == nil {
				return
			}
			switch e := ev.(type) {
			case agentcore.ToolExecutionStartEvent:
				started[e.ToolCallID] = time.Now()
				args, _ := e.Args.(json.RawMessage)
				emit(streamEvent{Type: "tool", Tool: e.ToolName, Phase: "start", ID: e.ToolCallID, Detail: s.toolDetail(agentCtx.Tools, e.ToolName, args)})
			case agentcore.ToolExecutionUpdateEvent:
				if tail := outputTail(resultText(e.PartialResult)); tail != "" {
					emit(streamEvent{Type: "tool", Tool: e.ToolName, Phase: "output", ID: e.ToolCallID, Text: tail})
				}
			case agentcore.ToolExecutionEndEvent:
				var elapsed int64
				if at, ok := started[e.ToolCallID]; ok {
					elapsed = time.Since(at).Milliseconds()
					delete(started, e.ToolCallID)
				}
				emit(streamEvent{Type: "tool", Tool: e.ToolName, Phase: "end", ID: e.ToolCallID, IsError: e.IsError,
					Text: toolFailure(resultText(e.Result), e.IsError), ElapsedMs: elapsed})
			case agentcore.CompactionStartEvent:
				compacting = &compactionReport{Reason: e.Reason, Before: e.TokensBefore}
				emit(streamEvent{Type: "compaction", Phase: "start", Compaction: compacting})
			case agentcore.CompactionEvent:
				compacting = nil
				emit(compactionEnd(e))
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
	s.checkpoint(managed, agentCtx.Messages)
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

// checkpoint appends the step's messages to the transcript file. It is called
// from the agent loop's goroutine while the message list is stable, and writes
// no database row: the session's metadata and the turn's usage are committed
// once, when the turn ends. A closed session is not written: it is being
// deleted.
//
// It reports whether the file was rewritten rather than appended to, which
// moves every entry's position.
func (s *apiServer) checkpoint(managed *managedSession, msgs agentcore.MessageList) (rewrote bool) {
	managed.mu.Lock()
	defer managed.mu.Unlock()
	return s.checkpointLocked(managed, msgs)
}

// checkpointLocked is checkpoint for a caller that holds managed.mu.
func (s *apiServer) checkpointLocked(managed *managedSession, msgs agentcore.MessageList) (rewrote bool) {
	if managed.closed {
		return false
	}
	rewrote, err := s.saveTranscript(managed, msgs)
	if err != nil {
		log.Printf("pigo-server: session %s: save transcript: %v", shortID(managed.meta.ID), err)
	}
	return rewrote
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
	thinking := agentcore.ThinkingLevel(managed.meta.Thinking)
	if thinking == "" {
		thinking = agentcore.ThinkingMedium
	}
	managed.runCfg.Model = managed.meta.Model
	managed.runCfg.Provider = providerName
	managed.runCfg.ThinkingLevel = thinking
	s.meterStreams(managed, prov, providerName)
	managed.runCfg.GetAPIKey = s.apiKeyFunc(managed.meta.UserID)
	// The prompt states the model's knowledge cutoff, so switching model
	// updates it. Doing so costs the prefix cache, which switching model has
	// already given up anyway; nothing else in the prompt moves.
	if key := priceKey(providerName, managed.meta.Model); key != managed.promptKey {
		if next := s.knowledgeCutoff(providerName, managed.meta.Model); next != managed.promptCutoff {
			prompt, ok := swapCutoffNote(managed.agentCtx.SystemPrompt, managed.promptCutoff, next)
			if !ok {
				if prompt, err = s.sessionPrompt(managed, managed.agentCtx.Tools, next); err != nil {
					return err
				}
			}
			managed.agentCtx.SystemPrompt = prompt
			managed.promptCutoff = next
		}
		managed.promptKey = key
	}
	return nil
}

// meterStreams points the session's model calls at the provider through the
// billing meter and the stall guard: the conversation's calls, and the summary calls that compact a
// long context (which otherwise fall back to the same unmetered stream).
func (s *apiServer) meterStreams(managed *managedSession, prov provider.Provider, providerName string) {
	// The stall guard is outermost, so when it abandons a call the meter
	// still records it (as aborted) on the way out. Tool naming is innermost:
	// only the provider sees the model-facing names.
	stream := s.nameStream(s.conversationStream(provider.StreamFnFromProvider(prov), providerName, managed.meta.ID), providerName)
	idle := streamIdleTimeout()
	managed.runCfg.Stream = guardStream(s.meter.wrap(stream, providerName, "chat"), idle)
	managed.runCfg.SummaryStream = guardStream(s.meter.wrap(stream, providerName, "compaction"), idle)
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
	return s.credentialSource(userID, providerName)
}

// apiKeyFunc is a session's key lookup: the owner's own key, then the shared
// pool, then nothing — never the process environment (credentialSource). It is
// resolved on every call, so a key saved or deleted while a session is loaded
// applies from its next call, summary calls included.
func (s *apiServer) apiKeyFunc(userID string) func(context.Context, string) string {
	return func(_ context.Context, providerName string) string {
		return s.credentials.resolve(userID, providerName, s.settings.get().AllowUserKeys)
	}
}

// errNoProviderKey is why a turn is refused before it starts: its provider has
// no key this user can use. Without the check the provider's own error would
// point at an environment variable the server no longer reads.
func (s *apiServer) errNoProviderKey(userID, providerName string) error {
	if s.credentialSource(userID, providerName) != "none" {
		return nil
	}
	return fmt.Errorf("服务商 %s 没有可用的 API Key：在 设置 → 服务商 里填写个人 Key，或请管理员配置公共 Key", providerName)
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

// The task tool: the model hands one self-contained job to a sub-agent, which
// runs its own loop with its own context and tool set and returns a report.
//
// The web server assembles it itself rather than using runtime.NewTaskTool:
// that spec fixes the child's tools and takes a no-argument RunConfig factory,
// so a per-call agent type and model could not reach it. Driving the child loop
// here (the same StartRun/DrainStream the host loop uses) also puts the child's
// steps in the activity log and keeps internal/ unchanged.
//
// Everything the deployment's own rules decide is reused rather than rebuilt:
// keys resolve through the session user's credentials, calls go through the
// meter (as kind "subagent", billed to the turn), and the tools are the
// session's sandbox-bound ones.
//
// See spec/subagent-task.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/cli/run"
	"github.com/smallnest/pigo/internal/provider"
	"github.com/smallnest/pigo/internal/runtime"
)

// subagentReportLimit bounds what comes back into the parent's context.
const subagentReportLimit = 8000

// A sub-agent is picked by type, not by a permission level: what it may do is
// a property of the role, the way Claude Code's agent definitions and pigo's
// own skill sub-agents work. Asking the model to judge "does this job need a
// shell" while it writes the prompt gets it wrong — it dispatched "run ls -la"
// as read-only — while "who should do this" is a judgement it makes well.
type subagentType struct {
	Name    string
	Summary string
	// without are the tools this type does not get, by canonical name. A
	// read-oriented agent still runs commands — that is how anyone looks
	// around a workspace — and only loses the editing tools, which is how
	// Claude Code's Explore agent is drawn. A whitelist that also took bash
	// away had the model spend three turns hunting for a way to run `ls`.
	without map[string]bool
}

var subagentTypes = []subagentType{
	{
		Name:    "general-purpose",
		Summary: "General-purpose agent with the same tools you have: it can run commands, edit files and carry out multi-step work. Use it for anything that changes something or needs a shell.",
	},
	{
		Name: "explore",
		Summary: "Read-oriented agent: it reads files, searches the code and the web, and runs commands to look around, but it has no file-editing tools. " +
			"Use it to find or check things without spending your own context.",
		without: map[string]bool{"write": true, "edit": true},
	},
}

const defaultSubagentType = "general-purpose"

// findSubagentType resolves the requested type, empty meaning the default.
func findSubagentType(name string) (subagentType, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = defaultSubagentType
	}
	for _, t := range subagentTypes {
		if strings.EqualFold(t.Name, name) {
			return t, nil
		}
	}
	var known []string
	for _, t := range subagentTypes {
		known = append(known, t.Name)
	}
	return subagentType{}, fmt.Errorf("没有 %q 这种子代理，可用的是：%s", name, strings.Join(known, "、"))
}

// subagentTypeHelp lists the types for the tool description, as Claude Code's
// Task tool lists its agent types.
func subagentTypeHelp() string {
	var b strings.Builder
	b.WriteString("\n\nAvailable agent types:\n")
	for _, t := range subagentTypes {
		fmt.Fprintf(&b, "- %s: %s\n", t.Name, t.Summary)
	}
	return b.String()
}

var taskDescription = "Dispatch a sub-agent to carry out one self-contained job and report back. " +
	"The sub-agent runs its own loop with a fresh context — it sees none of this conversation — so the prompt must carry everything it needs, and it answers only once: its final report is what you get back. " +
	"It shares this session's sandbox and workspace, so avoid having two of them edit the same file. " +
	"Use it to keep a long search, a survey of many files, or an independent check out of your own context. " +
	"Several task calls in one message run in parallel." + subagentTypeHelp()

const taskSystemPrompt = "You are a sub-agent working on one delegated job. " +
	"You have your own context and tool set, and you cannot dispatch further sub-agents. " +
	"Do the job with the tools you have, then reply with a self-contained report: what you found or did, and anything the agent that dispatched you must know. " +
	"Your final message is all that is returned, so do not refer to work the reader cannot see."

var taskSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "prompt": {
      "type": "string",
      "description": "The complete job for the sub-agent. It shares none of this conversation, so state the goal, the context it needs and what to report back."
    },
    "description": {
      "type": "string",
      "description": "A short (3-5 word) label for the job, shown in the activity log."
    },
    "subagent_type": {
      "type": "string",
      "enum": ["general-purpose", "explore"],
      "description": "Which agent to dispatch; see the list in this tool's description. Defaults to general-purpose."
    },
    "model": {
      "type": "string",
      "description": "Optional model for the sub-agent; must be one the deployment allows. Omit to use this conversation's model."
    }
  },
  "required": ["prompt"],
  "additionalProperties": false
}`)

type taskArgs struct {
	Prompt      string `json:"prompt"`
	Description string `json:"description,omitempty"`
	Type        string `json:"subagent_type,omitempty"`
	Model       string `json:"model,omitempty"`
}

// subagentGate bounds how many sub-agents one session runs at once. It reads
// the limit at each acquire, so an administrator's change applies to the next
// spawn — a fixed-capacity channel could not.
type subagentGate struct {
	mu      sync.Mutex
	cond    *sync.Cond
	running int
}

func newSubagentGate() *subagentGate {
	g := &subagentGate{}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// acquire waits for a slot or the context's end.
func (g *subagentGate) acquire(ctx context.Context, limit int) error {
	if limit < 1 {
		limit = 1
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	// Wake the waiters when the context ends, so a cancelled turn does not
	// leave one parked on the condition.
	stop := context.AfterFunc(ctx, func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.cond.Broadcast()
	})
	defer stop()
	for g.running >= limit {
		if err := ctx.Err(); err != nil {
			return err
		}
		g.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	g.running++
	return nil
}

func (g *subagentGate) release() {
	g.mu.Lock()
	g.running--
	g.mu.Unlock()
	g.cond.Broadcast()
}

// subagentSettings is what an administrator sets for sub-agents. A zero value
// means the defaults below, so a deployment that never opens the panel gets
// working limits.
type subagentSettings struct {
	// Disabled removes the tool from every session.
	Disabled bool `json:"disabled,omitempty"`
	// MaxConcurrent, MaxSteps and TimeoutSeconds are per session, per
	// sub-agent and per sub-agent; zero means the default.
	MaxConcurrent  int `json:"maxConcurrent,omitempty"`
	MaxSteps       int `json:"maxSteps,omitempty"`
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
	// Models are the ids a call may ask for. Empty means only the
	// conversation's own model.
	Models    []string  `json:"models,omitempty"`
	UpdatedBy string    `json:"updatedBy,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

const (
	defaultSubagentConcurrent = 4
	defaultSubagentSteps      = 20
	defaultSubagentTimeout    = 5 * time.Minute

	maxSubagentConcurrent = 16
	maxSubagentSteps      = 200
	maxSubagentTimeout    = time.Hour
)

func (c subagentSettings) concurrent() int {
	if c.MaxConcurrent <= 0 {
		return defaultSubagentConcurrent
	}
	return c.MaxConcurrent
}

func (c subagentSettings) steps() int {
	if c.MaxSteps <= 0 {
		return defaultSubagentSteps
	}
	return c.MaxSteps
}

func (c subagentSettings) timeout() time.Duration {
	if c.TimeoutSeconds <= 0 {
		return defaultSubagentTimeout
	}
	return time.Duration(c.TimeoutSeconds) * time.Second
}

// allows reports whether a call may ask for this model.
func (c subagentSettings) allows(model string) bool {
	for _, item := range c.Models {
		if strings.EqualFold(strings.TrimSpace(item), model) {
			return true
		}
	}
	return false
}

// modelHint names the allowed models for the error the model reads.
func (c subagentSettings) modelHint() string {
	if len(c.Models) == 0 {
		return "（管理员没有配置可选模型，去掉 model 参数即可用当前会话的模型）"
	}
	return "（可用：" + strings.Join(c.Models, "、") + "）"
}

// validate normalizes a submitted configuration.
func (c subagentSettings) validate() (subagentSettings, error) {
	if c.MaxConcurrent < 0 || c.MaxConcurrent > maxSubagentConcurrent {
		return c, fmt.Errorf("并发上限须在 1 到 %d 之间", maxSubagentConcurrent)
	}
	if c.MaxSteps < 0 || c.MaxSteps > maxSubagentSteps {
		return c, fmt.Errorf("步数上限须在 1 到 %d 之间", maxSubagentSteps)
	}
	if c.TimeoutSeconds < 0 || time.Duration(c.TimeoutSeconds)*time.Second > maxSubagentTimeout {
		return c, fmt.Errorf("超时须在 %s 以内", maxSubagentTimeout)
	}
	var models []string
	seen := map[string]bool{}
	for _, item := range c.Models {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		models = append(models, item)
	}
	c.Models = models
	return c, nil
}

// subagent returns the deployment's configuration.
func (s *settingsStore) subagent() subagentSettings {
	if c := s.get().Subagent; c != nil {
		return *c
	}
	return subagentSettings{}
}

// setSubagent stores it.
func (s *settingsStore) setSubagent(next subagentSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.settings.Subagent
	s.settings.Subagent = &next
	if err := s.saveLocked(); err != nil {
		s.settings.Subagent = previous
		return err
	}
	return nil
}

// taskTool is one session's task tool.
type taskTool struct {
	server  *apiServer
	session *managedSession
}

func (t *taskTool) Name() string            { return "task" }
func (t *taskTool) Description() string     { return taskDescription }
func (t *taskTool) Schema() json.RawMessage { return taskSchema }

// ExecutionMode is parallel: sub-agents are independent runs. Their commands
// still queue on the session's one container shell.
func (t *taskTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}

// Detail is the activity log's one line for the call.
func (t *taskTool) Detail(raw json.RawMessage) string {
	var a taskArgs
	_ = json.Unmarshal(raw, &a)
	label := strings.TrimSpace(a.Description)
	if label == "" {
		label = firstLine(strings.TrimSpace(a.Prompt))
	}
	parts := []string{label}
	if kind := strings.TrimSpace(a.Type); kind != "" && kind != defaultSubagentType {
		parts = append(parts, kind)
	}
	if model := strings.TrimSpace(a.Model); model != "" {
		parts = append(parts, model)
	}
	return clip(strings.Join(parts, " · "), detailLimit)
}

func (t *taskTool) Execute(ctx context.Context, id string, raw json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	var a taskArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return agentcore.AgentToolResult{}, fmt.Errorf("task: 参数无法解析：%w", err)
		}
	}
	a.Prompt = strings.TrimSpace(a.Prompt)
	if a.Prompt == "" {
		return agentcore.AgentToolResult{}, errors.New("task: prompt 不能为空")
	}
	return t.server.runSubagent(ctx, t.session, id, a, onUpdate)
}

// runSubagent is one spawn: it resolves the model, builds the child's tools and
// configuration, runs the child loop, and returns its report.
func (s *apiServer) runSubagent(ctx context.Context, managed *managedSession, callID string, a taskArgs, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	limits := s.settings.subagent()
	managed.mu.Lock()
	userID, parentModel, parentProvider := managed.meta.UserID, managed.meta.Model, managed.meta.Provider
	thinking := managed.meta.Thinking
	gate := managed.subagentGate()
	managed.mu.Unlock()

	model, providerName := parentModel, parentProvider
	// Naming the conversation's own model is always allowed: it is what the
	// call gets by leaving the field out, and refusing it reads as arbitrary.
	if want := strings.TrimSpace(a.Model); want != "" && !strings.EqualFold(want, parentModel) {
		if !limits.allows(want) {
			return agentcore.AgentToolResult{}, fmt.Errorf("task: 模型 %s 不在允许的清单里%s", want, limits.modelHint())
		}
		model = want
		providerName = ""
	}
	prov, resolved, err := s.resolveModel(userID, model, providerName)
	if err != nil {
		return agentcore.AgentToolResult{}, fmt.Errorf("task: %w", err)
	}
	providerName = resolved
	if err := s.errNoProviderKey(userID, providerName); err != nil {
		return agentcore.AgentToolResult{}, fmt.Errorf("task: %w", err)
	}

	kind, err := findSubagentType(a.Type)
	if err != nil {
		return agentcore.AgentToolResult{}, fmt.Errorf("task: %w", err)
	}
	tools, err := s.subagentTools(managed, kind, limits.steps())
	if err != nil {
		return agentcore.AgentToolResult{}, fmt.Errorf("task: %w", err)
	}

	if err := gate.acquire(ctx, limits.concurrent()); err != nil {
		return agentcore.AgentToolResult{}, fmt.Errorf("task: %w", err)
	}
	defer gate.release()

	childCtx, cancel := context.WithTimeout(ctx, limits.timeout())
	defer cancel()

	cfg := run.NewConfig(model, providerName, agentcore.ThinkingLevel(thinking), prov, provider.NewCredentialStore(nil), run.ToolRegistry(tools), run.TodoReminders(tools), nil)
	cfg.GetAPIKey = s.apiKeyFunc(userID)
	// The child's own chain: the naming profile and conversation id follow the
	// provider and model it actually runs on, and its calls are metered as
	// "subagent" under this turn, billed by its own key's tier.
	c := call{provider: providerName, kind: "subagent", keySource: s.keySource(userID, providerName)}
	stream := s.nameStream(s.conversationStream(provider.StreamFnFromProvider(prov), providerName, managed.meta.ID+"#"+callID), providerName)
	idle := streamIdleTimeout()
	cfg.Stream = guardStream(s.meter.wrapAs(stream, c), idle, streamFirstByteTimeout(idle), "subagent:"+shortID(callID))
	cfg.SummaryStream = cfg.Stream
	params := s.contextParams(providerName, model)
	cfg.ContextWindow = params.Window
	cfg.Compaction = params.settings()

	// The child's own environment: it may run on a different model from the
	// parent's, so the date and the cutoff are its model's, not the session's.
	childPrompt := taskSystemPrompt + subagentEnvironment(time.Now(), s.knowledgeCutoff(providerName, model))

	agentCtx := &agentcore.AgentContext{
		SystemPrompt: childPrompt,
		Messages: agentcore.MessageList{agentcore.UserMessage{
			RoleField: agentcore.RoleUser,
			Content:   agentcore.ContentList{agentcore.NewTextContent(a.Prompt)},
		}},
		Tools: tools,
	}

	report, err := s.driveSubagent(childCtx, agentCtx, cfg, callID, onUpdate)
	report = clipReport(report)
	switch {
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		note := fmt.Sprintf("子代理超过 %s 的时间上限，以下是中断前的进展：", limits.timeout())
		return textResult(strings.TrimSpace(note + "\n\n" + report)), nil
	case err != nil:
		if report != "" {
			return textResult(report), fmt.Errorf("task: 子代理中断：%w", err)
		}
		return agentcore.AgentToolResult{}, fmt.Errorf("task: 子代理失败：%w", err)
	case strings.TrimSpace(report) == "":
		return textResult("子代理没有给出报告。"), nil
	}
	return textResult(report), nil
}

// driveSubagent runs the child loop, reporting its steps into the parent turn's
// activity log under callID, and returns the child's final report.
func (s *apiServer) driveSubagent(ctx context.Context, agentCtx *agentcore.AgentContext, cfg runtime.RunConfig, callID string, onUpdate agentcore.ToolUpdateFunc) (string, error) {
	run := runFrom(ctx)
	// publish puts a child event under the task call; without a turn (tests,
	// a detached run) the child still runs, just unobserved.
	publish := func(ev streamEvent) {
		if run != nil {
			ev.ParentID = callID
			run.publish(ev)
		}
	}
	// progress keeps the task's own line showing what the child is doing.
	progress := func(text string) {
		if onUpdate != nil && strings.TrimSpace(text) != "" {
			onUpdate(textResult(clip(text, detailLimit)))
		}
	}
	started := map[string]time.Time{}
	// say holds what the child last wrote. Like the turn's own log, the text
	// before a call is that step's narration and lands ahead of it; what is
	// left when the run ends is the report, which comes back as the result
	// instead. It is taken from the message events rather than OnText, which
	// only flushes at the end of a turn — after that turn's calls have run.
	say := ""
	narrate := func() {
		text := strings.TrimSpace(say)
		say = ""
		if text == "" {
			return
		}
		publish(streamEvent{Type: "subagent", Phase: "text", Text: text})
		progress(firstLine(text))
	}
	stream := runtime.StartRun(ctx, agentCtx, cfg)
	final, err := runtime.DrainStream(ctx, stream, runtime.StreamHandler{
		OnEvent: func(ev agentcore.AgentEvent) {
			switch e := ev.(type) {
			case agentcore.MessageUpdateEvent:
				// The message as it streams: by the time a call starts, this
				// holds what the child wrote before it. MessageEndEvent is too
				// late — the loop starts the call before the message ends.
				if msg, ok := e.Message.(agentcore.AssistantMessage); ok {
					if text := agentcore.ContentToText(msg.Content); text != "" {
						say = text
					}
				}
			case agentcore.ToolExecutionStartEvent:
				narrate()
				started[e.ToolCallID] = time.Now()
				args, _ := e.Args.(json.RawMessage)
				detail := s.toolDetail(agentCtx.Tools, e.ToolName, args)
				publish(streamEvent{Type: "tool", Tool: e.ToolName, Phase: "start", ID: e.ToolCallID, Detail: detail})
				progress(e.ToolName + " " + detail)
			case agentcore.ToolExecutionEndEvent:
				// A call the loop rejected never started, so its narration is
				// still pending: flush it here too, or it would be overwritten
				// by what the child writes next.
				narrate()
				var elapsed int64
				if at, ok := started[e.ToolCallID]; ok {
					elapsed = time.Since(at).Milliseconds()
					delete(started, e.ToolCallID)
				}
				publish(streamEvent{Type: "tool", Tool: e.ToolName, Phase: "end", ID: e.ToolCallID, IsError: e.IsError,
					Text: toolFailure(resultText(e.Result), e.IsError), ElapsedMs: elapsed})
			}
		},
	})
	text := ""
	if final != nil {
		text = strings.TrimSpace(agentcore.ContentToText(final.Content))
	}
	return text, err
}

// clipReport bounds what a report adds to the parent's context. The limit is
// in runes, not bytes: a report is usually Chinese, and cutting mid-rune would
// put a broken character into the transcript and the parent's context.
func clipReport(text string) string {
	if utf8.RuneCountInString(text) <= subagentReportLimit {
		return text
	}
	return string([]rune(text)[:subagentReportLimit]) + "\n\n（已截断，子代理的原文更长）"
}

// subagentTools is the child's tool set: the session's own tools without task
// (a sub-agent cannot dispatch further ones), narrowed to what its type may
// use, each call counting against the step limit.
func (s *apiServer) subagentTools(managed *managedSession, kind subagentType, maxSteps int) ([]agentcore.AgentTool, error) {
	tools, err := s.hostTools(managed)
	if err != nil {
		return nil, err
	}
	steps := &stepCounter{limit: maxSteps}
	out := make([]agentcore.AgentTool, 0, len(tools))
	for _, tool := range tools {
		if tool.Name() == "task" {
			continue
		}
		if kind.without[tool.Name()] {
			continue
		}
		out = append(out, &countedTool{AgentTool: tool, steps: steps})
	}
	if len(out) == 0 {
		return nil, errors.New("子代理没有可用的工具")
	}
	return out, nil
}

// stepCounter caps how much work one sub-agent does.
type stepCounter struct {
	mu    sync.Mutex
	used  int
	limit int
}

// take reports whether another call is allowed.
func (c *stepCounter) take() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.limit > 0 && c.used >= c.limit {
		return false
	}
	c.used++
	return true
}

// countedTool is one of the child's tools, counting against the step limit.
// Past the limit it refuses rather than the run being cancelled, so the child
// still writes its report — a cancelled sub-agent returns nothing.
type countedTool struct {
	agentcore.AgentTool
	steps *stepCounter
}

func (t *countedTool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	if !t.steps.take() {
		return textResult("已达子代理的步数上限，不再执行工具。请用目前掌握的信息给出结论。"), nil
	}
	return t.AgentTool.Execute(ctx, id, args, onUpdate)
}

// Detail keeps the wrapped tool's activity line.
func (t *countedTool) Detail(args json.RawMessage) string {
	if d, ok := t.AgentTool.(interface{ Detail(json.RawMessage) string }); ok {
		return d.Detail(args)
	}
	return ""
}

// --- HTTP -----------------------------------------------------------------------

// subagentResponse is the panel's view: the settings, plus the defaults it
// shows as placeholders when a field is left at zero.
type subagentResponse struct {
	Settings subagentSettings `json:"settings"`
	Defaults struct {
		MaxConcurrent  int `json:"maxConcurrent"`
		MaxSteps       int `json:"maxSteps"`
		TimeoutSeconds int `json:"timeoutSeconds"`
	} `json:"defaults"`
}

func (s *apiServer) subagentResponse() subagentResponse {
	out := subagentResponse{Settings: s.settings.subagent()}
	out.Defaults.MaxConcurrent = defaultSubagentConcurrent
	out.Defaults.MaxSteps = defaultSubagentSteps
	out.Defaults.TimeoutSeconds = int(defaultSubagentTimeout / time.Second)
	return out
}

func (s *apiServer) handleSubagent(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.subagentResponse())
}

func (s *apiServer) handlePutSubagent(w http.ResponseWriter, r *http.Request) {
	var request subagentSettings
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	next, err := request.validate()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	next.UpdatedBy = principalForRequest(r).Username
	next.UpdatedAt = time.Now().UTC()
	if err := s.settings.setSubagent(next); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logAdminAction(r, "set subagent limits", fmt.Sprintf("enabled=%v concurrent=%d steps=%d timeout=%s models=%d",
		!next.Disabled, next.concurrent(), next.steps(), next.timeout(), len(next.Models)))
	writeJSON(w, http.StatusOK, s.subagentResponse())
}

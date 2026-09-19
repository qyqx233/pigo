package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
)

// sandboxBashTool runs bash inside the session's long-lived bwrap. It never
// sees orchestrator credentials: the sandbox env is cleared of provider keys.
type sandboxBashTool struct {
	server  *apiServer
	session *managedSession
}

type sandboxBashArgs struct {
	Command         string `json:"command"`
	TimeoutMs       int    `json:"timeout_ms,omitempty"`
	RunInBackground bool   `json:"run_in_background,omitempty"`
}

func (t *sandboxBashTool) Name() string { return "bash" }

func (t *sandboxBashTool) Description() string {
	return "Run a shell command inside the session sandbox. " +
		"A non-zero exit code is reported as an error. Background jobs are not supported."
}

func (t *sandboxBashTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "command":    {"type": "string", "description": "Shell command line to run."},
    "timeout_ms": {"type": "integer", "description": "Timeout in milliseconds (capped at 10 minutes).", "minimum": 0}
  },
  "required": ["command"],
  "additionalProperties": false
}`)
}

func (t *sandboxBashTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionSequential
}

func (t *sandboxBashTool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	var a sandboxBashArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return textResult("bash: invalid arguments"), fmt.Errorf("bash: invalid arguments: %w", err)
	}
	a.Command = strings.TrimSpace(a.Command)
	if a.Command == "" {
		return textResult("bash: command is required"), fmt.Errorf("bash: command is required")
	}
	if a.RunInBackground {
		return textResult("bash: run_in_background is not supported in the web sandbox"), fmt.Errorf("bash: background jobs are not supported")
	}
	timeout := 2 * time.Minute
	if a.TimeoutMs > 0 {
		timeout = time.Duration(a.TimeoutMs) * time.Millisecond
	}
	if timeout > 10*time.Minute {
		timeout = 10 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// The session lock guards the sandbox pointer, not the command: it is held
	// to find (or start) the sandbox and released before the command runs, so
	// the session stays responsive during a long command.
	t.session.mu.Lock()
	if t.session.closed {
		t.session.mu.Unlock()
		return textResult("bash: session closed"), fmt.Errorf("bash: session closed")
	}
	spec := t.server.sandboxSpec(t.session)
	if err := t.server.ensureLive(t.session, spec); err != nil {
		t.session.mu.Unlock()
		return textResult("bash: sandbox unavailable"), err
	}
	live := t.session.live
	t.session.mu.Unlock()
	res, err := runLiveJobStream(runCtx, live, a.Command, progressReporter(onUpdate))
	out := clipOutput(strings.TrimRight(res.Stdout+res.Stderr, "\n"))
	if onUpdate != nil {
		onUpdate(textResult(out))
	}
	// Tell apart the command's own timeout from the turn being stopped around
	// it: runCtx is derived from the turn's context, so both end it.
	if reason := stopReason(ctx); reason != "" {
		return textResult(out), fmt.Errorf("bash: the turn was stopped (%s); the command was terminated\n%s", reason, out)
	}
	if runCtx.Err() == context.DeadlineExceeded {
		return textResult(out), fmt.Errorf("bash: command timed out after %s (its own timeout)\n%s", timeout, out)
	}
	if err != nil {
		return textResult(out), fmt.Errorf("bash: %v\n%s", err, out)
	}
	if res.Exit != 0 {
		return agentcore.AgentToolResult{
			Content: agentcore.ContentList{agentcore.NewTextContent(out)},
			Details: map[string]any{"exitCode": res.Exit},
		}, fmt.Errorf("bash: command exited with code %d\n%s", res.Exit, out)
	}
	return agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent(out)},
		Details: map[string]any{"exitCode": 0},
	}, nil
}

func textResult(s string) agentcore.AgentToolResult {
	return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent(s)}}
}

package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/smallnest/pigo/internal/agentcore"
)

const virtualWorkspaceRoot = "/workspace"

// virtualWorkspaceTool lets the model use the same /workspace paths for
// host-side file tools that bash sees inside bwrap. The wrapped tool still owns
// path resolution and boundary checks; this adapter only removes the virtual
// mount prefix from a JSON "path" argument.
type virtualWorkspaceTool struct {
	inner agentcore.AgentTool
}

func withVirtualWorkspace(inner agentcore.AgentTool) agentcore.AgentTool {
	return &virtualWorkspaceTool{inner: inner}
}

func (t *virtualWorkspaceTool) Name() string { return t.inner.Name() }

func (t *virtualWorkspaceTool) Description() string { return t.inner.Description() }

func (t *virtualWorkspaceTool) Schema() json.RawMessage { return t.inner.Schema() }

func (t *virtualWorkspaceTool) ExecutionMode() agentcore.ToolExecutionMode {
	return t.inner.ExecutionMode()
}

func (t *virtualWorkspaceTool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	return t.inner.Execute(ctx, id, rewriteVirtualWorkspacePath(args), onUpdate)
}

func rewriteVirtualWorkspacePath(args json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(args, &fields) != nil {
		return args
	}
	rawPath, ok := fields["path"]
	if !ok {
		return args
	}
	var requested string
	if json.Unmarshal(rawPath, &requested) != nil {
		return args
	}
	cleaned := filepath.Clean(requested)
	if cleaned != virtualWorkspaceRoot && !strings.HasPrefix(cleaned, virtualWorkspaceRoot+string(filepath.Separator)) {
		return args
	}
	relative, err := filepath.Rel(virtualWorkspaceRoot, cleaned)
	if err != nil {
		return args
	}
	encoded, err := json.Marshal(relative)
	if err != nil {
		return args
	}
	fields["path"] = encoded
	rewritten, err := json.Marshal(fields)
	if err != nil {
		return args
	}
	return rewritten
}

func virtualizeWorkspacePrompt(prompt, hostWorkspace string) string {
	if strings.TrimSpace(hostWorkspace) == "" {
		return prompt
	}
	prompt = strings.ReplaceAll(prompt, hostWorkspace, virtualWorkspaceRoot)
	return prompt + "\n\nWorkspace paths are rooted at /workspace. Use relative paths or /workspace/... " +
		"with file tools; bash uses the same workspace path inside its sandbox."
}

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/agenttool"
)

func TestRewriteVirtualWorkspacePath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "root", path: "/workspace", want: "."},
		{name: "absolute child", path: "/workspace/src/main.go", want: "src/main.go"},
		{name: "relative unchanged", path: "src/main.go", want: "src/main.go"},
		{name: "other absolute unchanged", path: "/etc/passwd", want: "/etc/passwd"},
		{name: "similar prefix unchanged", path: "/workspace-other/file", want: "/workspace-other/file"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input, err := json.Marshal(map[string]any{"path": test.path, "offset": 2})
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
			}
			if err := json.Unmarshal(rewriteVirtualWorkspacePath(input), &got); err != nil {
				t.Fatal(err)
			}
			if got.Path != test.want || got.Offset != 2 {
				t.Fatalf("rewrite path = %q offset = %d, want %q and 2", got.Path, got.Offset, test.want)
			}
		})
	}
}

func TestVirtualWorkspaceToolReadsMountedPath(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := withVirtualWorkspace(&agenttool.ReadTool{Root: root})
	result, err := tool.Execute(context.Background(), "call", json.RawMessage(`{"path":"/workspace/note.txt"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if textResultContent(result) != "     1\thello\n" {
		t.Fatalf("result = %q", textResultContent(result))
	}

	escape, err := tool.Execute(context.Background(), "call", json.RawMessage(`{"path":"/workspace/../secret"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(textResultContent(escape), "outside the workspace root") {
		t.Fatalf("escape result = %q", textResultContent(escape))
	}
}

func TestVirtualizeWorkspacePrompt(t *testing.T) {
	host := "/host/private/session/workspace"
	got := virtualizeWorkspacePrompt("Environment:\n- Working directory: "+host+"\n# "+host+"/AGENTS.md", host)
	if strings.Contains(got, host) {
		t.Fatalf("host workspace leaked in prompt: %s", got)
	}
	if strings.Count(got, virtualWorkspaceRoot) < 2 {
		t.Fatalf("virtual workspace missing from prompt: %s", got)
	}
}

func textResultContent(result agentcore.AgentToolResult) string {
	var b strings.Builder
	for _, content := range result.Content {
		if text, ok := content.(agentcore.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

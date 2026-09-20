package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smallnest/pigo/cmd/pigo-server/ext/allowfetch"
	"github.com/smallnest/pigo/internal/agentcore"
)

const commandToolsConfig = `
tools:
  - name: echo_args
    description: Echo the arguments.
    command: cat
  - name: show_secret
    description: Print the token.
    command: 'printf "%s|%s|%s" "$TOKEN" "$PIGO_TOOL_NAME" "$PIGO_TOOL_ARGS"'
    env:
      TOKEN: ${EXT_TEST_TOKEN}
  - name: read_tmp
    description: Read a file bash left in /tmp.
    command: cat /tmp/from-bash
  - name: fail
    description: Fail.
    command: 'echo partial; echo "it broke" >&2; exit 3'
  - name: slow
    description: Take too long.
    command: sleep 30
    timeout: 1s
`

// sandboxedServer is a test server whose sessions get a real bwrap container.
func sandboxedServer(t *testing.T, config string) (*apiServer, *managedSession) {
	t.Helper()
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bwrap not available")
	}
	x, err := parseToolsConfig([]byte(config), testEnv(map[string]string{"EXT_TEST_TOKEN": "tok-123"}))
	if err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t)
	server.noTools = false
	server.toolNames = builtinToolNames
	server.loopFn = nil
	server.sandbox = Sandbox{Bwrap: bwrap, Pigo: filepath.Join(t.TempDir(), "unused")}
	server.exts = x
	id := createSession(t, server)
	managed, _ := server.getSession(id)
	t.Cleanup(func() {
		managed.mu.Lock()
		if managed.live != nil {
			managed.live.stop()
		}
		managed.mu.Unlock()
	})
	return server, managed
}

func runExtTool(t *testing.T, server *apiServer, managed *managedSession, name, args string) (string, error) {
	t.Helper()
	spec, ok := server.exts.tool(name)
	if !ok {
		t.Fatalf("no tool %s", name)
	}
	tool := &extCommandTool{spec: spec, server: server, session: managed}
	res, err := tool.Execute(context.Background(), "call-1", json.RawMessage(args), nil)
	return resultText(res), err
}

func TestCommandToolInSessionContainer(t *testing.T) {
	server, managed := sandboxedServer(t, commandToolsConfig)

	// Arguments arrive on stdin.
	if out, err := runExtTool(t, server, managed, "echo_args", `{"path":"a.xlsx"}`); err != nil || out != `{"path":"a.xlsx"}` {
		t.Errorf("echo_args = %q %v", out, err)
	}

	// The env reaches the command, with the tool's name and arguments.
	if out, err := runExtTool(t, server, managed, "show_secret", `{"q":1}`); err != nil || out != `tok-123|show_secret|{"q":1}` {
		t.Errorf("show_secret = %q %v", out, err)
	}
	// ...and only that command: bash in the same container does not have it.
	managed.mu.Lock()
	live := managed.live
	managed.mu.Unlock()
	if res, err := runLiveJob(context.Background(), live, `printf '[%s]' "$TOKEN"`); err != nil || res.Stdout != "[]" {
		t.Errorf("bash sees TOKEN: %q %v", res.Stdout, err)
	}

	// Same container as bash: a file bash leaves in /tmp is there.
	if _, err := runLiveJob(context.Background(), live, "printf left-by-bash > /tmp/from-bash"); err != nil {
		t.Fatal(err)
	}
	if out, err := runExtTool(t, server, managed, "read_tmp", `{}`); err != nil || out != "left-by-bash" {
		t.Errorf("read_tmp = %q %v", out, err)
	}

	// A failure: stdout and stderr both reach the model, apart, and the
	// exit code is reported the way bash's is.
	out, err := runExtTool(t, server, managed, "fail", `{}`)
	if err == nil || !strings.Contains(err.Error(), "command exited with code 3") || out != "partial\nit broke" {
		t.Errorf("fail = %q %v", out, err)
	}

	// No files are left behind in the session home.
	if matches, _ := filepath.Glob(filepath.Join(managed.paths.Home, runDirName, "*")); len(matches) != 0 {
		t.Errorf("left behind: %v", matches)
	}

	// A timeout ends the command (and, as with bash, the container)...
	if _, err := runExtTool(t, server, managed, "slow", `{}`); err == nil || !strings.Contains(err.Error(), "timed out after 1s") {
		t.Errorf("slow = %v", err)
	}
	// ...and the next call starts a new one.
	if out, err := runExtTool(t, server, managed, "echo_args", `{"again":true}`); err != nil || out != `{"again":true}` {
		t.Errorf("after restart = %q %v", out, err)
	}
}

// fakeWebFetch stands in for the built-in, recording what reaches it.
type fakeWebFetch struct {
	agentcore.AgentTool
	calls []string
}

func (f *fakeWebFetch) Name() string            { return "webfetch" }
func (f *fakeWebFetch) Description() string     { return "Fetch a page." }
func (f *fakeWebFetch) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (f *fakeWebFetch) Execute(_ context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	f.calls = append(f.calls, string(args))
	return textResult("page"), nil
}

func TestAllowFetchExtension(t *testing.T) {
	builtin := &fakeWebFetch{}
	if _, err := allowfetch.New(nil, nil, map[string]string{"ALLOW_HOSTS": "a.com"}); err == nil {
		t.Error("built without a tool to wrap")
	}
	if _, err := allowfetch.New(nil, builtin, nil); err == nil {
		t.Error("built without ALLOW_HOSTS")
	}
	tool, err := allowfetch.New(nil, builtin, map[string]string{"ALLOW_HOSTS": "docs.example.com, *.example.org"})
	if err != nil {
		t.Fatal(err)
	}
	for url, allowed := range map[string]bool{
		"https://docs.example.com/x": true,
		"https://api.example.org/y":  true,
		"https://example.org/":       false,
		"https://evil.com/":          false,
	} {
		_, err := tool.Execute(context.Background(), "c", json.RawMessage(`{"url":"`+url+`"}`), nil)
		if (err == nil) != allowed {
			t.Errorf("%s: err = %v, want allowed=%v", url, err, allowed)
		}
	}
	if len(builtin.calls) != 2 {
		t.Errorf("delegated %d calls, want 2", len(builtin.calls))
	}
	if !strings.Contains(tool.Description(), "docs.example.com") {
		t.Errorf("description = %q", tool.Description())
	}
}

func TestApplyExtensions(t *testing.T) {
	x, err := parseToolsConfig([]byte(fullToolsConfig), testEnv(map[string]string{"SEARCH_TOKEN": "x"}))
	if err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t)
	server.noTools = false
	server.exts = x
	id := createSession(t, server)
	managed, _ := server.getSession(id)

	tools, err := server.hostTools(managed)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]agentcore.AgentTool{}
	var order []string
	for _, tool := range tools {
		if _, dup := byName[tool.Name()]; dup {
			t.Errorf("%s twice", tool.Name())
		}
		byName[tool.Name()] = tool
		order = append(order, tool.Name())
	}
	if _, ok := byName["websearch"].(*extCommandTool); !ok {
		t.Errorf("websearch is %T, want the command tool", byName["websearch"])
	}
	fetch, ok := byName["webfetch"].(*configuredTool)
	if !ok || !strings.Contains(fetch.Description(), "example.com") {
		t.Errorf("webfetch is %T", byName["webfetch"])
	}
	// Replacements stay in place, additions come last — then the task tool,
	// which hostTools adds after the extensions.
	if order[len(order)-1] != "task" || order[len(order)-2] != "xlsx_summary" || order[0] != "read" {
		t.Errorf("order = %v (replacements in place, additions last)", order)
	}

	// -tools picks from the lot, extensions included.
	server.config.tools = "read,xlsx_summary"
	server.toolNames = []string{"read", "xlsx_summary"}
	tools, _ = server.hostTools(managed)
	if len(tools) != 2 || tools[0].Name() != "read" || tools[1].Name() != "xlsx_summary" {
		t.Errorf("selected = %v", tools)
	}
}

func TestExtensionToolDetail(t *testing.T) {
	x, err := parseToolsConfig([]byte(fullToolsConfig), testEnv(map[string]string{"SEARCH_TOKEN": "x"}))
	if err != nil {
		t.Fatal(err)
	}
	s := &apiServer{exts: x}
	fetch, _ := allowfetch.New(nil, &fakeWebFetch{}, map[string]string{"ALLOW_HOSTS": "a.com"})
	tools := []agentcore.AgentTool{&configuredTool{AgentTool: fetch, name: "webfetch"}}
	cases := []struct {
		name, args, want string
	}{
		{"xlsx_summary", `{"sheet":"S1","path":"data/a.xlsx"}`, "data/a.xlsx"}, // template
		{"webfetch", `{"url":"https://a.com/x"}`, "https://a.com/x"},           // Detailer
		{"websearch", `{"query":"pigo"}`, "pigo"},                              // the built-in's rule
		{"bash", `{"command":"ls -la"}`, "ls -la"},                             // plain built-in
	}
	for _, tc := range cases {
		if got := s.toolDetail(tools, tc.name, json.RawMessage(tc.args)); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	// An added tool without a template shows its first string argument.
	x.byName["plain"] = &extToolSpec{Name: "plain", Command: "x"}
	if got := s.toolDetail(nil, "plain", json.RawMessage(`{"n":1,"b":"second","a":"third"}`)); got != "second" {
		t.Errorf("first string = %q", got)
	}
}

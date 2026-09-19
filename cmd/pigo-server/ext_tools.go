// Extension tools in a session: the config's command tools, which run in the
// session's sandbox container the way bash does, and its Go tools, built by
// the registered factories. Both are assembled into the session's tool set
// here, replacing a built-in of the same name or joining the set.
//
// See spec/tool-extensions.md.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/smallnest/pigo/cmd/pigo-server/ext"
	"github.com/smallnest/pigo/internal/agentcore"
)

// runDirName is the directory under the session home that carries a
// command's stdin and stderr in and out of the container: the container's
// shell reads commands from its own stdin and merges their stderr into
// stdout, so both go through files instead.
const runDirName = ".pigo-run"

// maxArgsEnv caps PIGO_TOOL_ARGS; larger arguments arrive on stdin only.
const maxArgsEnv = 32 << 10

// runInSession runs a command in the session's sandbox container, starting it
// if needed.
func (s *apiServer) runInSession(ctx context.Context, managed *managedSession, req ext.RunRequest) (ext.RunResult, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultExtTimeout
	}
	if timeout > maxExtTimeout {
		timeout = maxExtTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// As with bash, the lock covers finding (or starting) the container, not
	// the command.
	managed.mu.Lock()
	if managed.closed {
		managed.mu.Unlock()
		return ext.RunResult{}, errors.New("session closed")
	}
	if err := s.ensureLive(managed, s.sandboxSpec(managed)); err != nil {
		managed.mu.Unlock()
		return ext.RunResult{}, fmt.Errorf("sandbox unavailable: %w", err)
	}
	live := managed.live
	managed.mu.Unlock()

	dir := filepath.Join(managed.paths.Home, runDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ext.RunResult{}, err
	}
	id, err := randomID()
	if err != nil {
		return ext.RunResult{}, err
	}
	inHost, errHost := filepath.Join(dir, id+".in"), filepath.Join(dir, id+".err")
	defer os.Remove(inHost)
	defer os.Remove(errHost)
	if err := os.WriteFile(inHost, req.Stdin, 0o600); err != nil {
		return ext.RunResult{}, err
	}
	inBox, errBox := "/home/pigo/"+runDirName+"/"+id+".in", "/home/pigo/"+runDirName+"/"+id+".err"

	var line strings.Builder
	line.WriteString("env")
	keys := make([]string, 0, len(req.Env))
	for k := range req.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		line.WriteString(" " + shQuote(k+"="+req.Env[k]))
	}
	line.WriteString(" /bin/sh -c " + shQuote(req.Command) + " <" + shQuote(inBox) + " 2>" + shQuote(errBox))

	res, runErr := runLiveJobStream(runCtx, live, line.String(), req.OnOutput)
	stderr, _ := os.ReadFile(errHost)
	if len(stderr) > 16<<10 {
		stderr = stderr[len(stderr)-16<<10:]
	}
	return ext.RunResult{Stdout: res.Stdout, Stderr: string(stderr), Exit: res.Exit}, runErr
}

// extSession is what a Go extension sees of a session.
type extSession struct {
	server  *apiServer
	managed *managedSession
}

func (e extSession) ID() string        { return e.managed.meta.ID }
func (e extSession) UserID() string    { return e.managed.meta.UserID }
func (e extSession) Workspace() string { return e.managed.paths.Workspace }
func (e extSession) Run(ctx context.Context, req ext.RunRequest) (ext.RunResult, error) {
	return e.server.runInSession(ctx, e.managed, req)
}

// extCommandTool is a command tool from the config.
type extCommandTool struct {
	spec    *extToolSpec
	server  *apiServer
	session *managedSession
}

func (t *extCommandTool) Name() string            { return t.spec.Name }
func (t *extCommandTool) Description() string     { return t.spec.Description }
func (t *extCommandTool) Schema() json.RawMessage { return t.spec.schema }

// ExecutionMode is sequential for the reason bash's is: commands share the
// container's one shell, and two written into it at once would mix.
func (t *extCommandTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionSequential
}

func (t *extCommandTool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	name := t.spec.Name
	env := map[string]string{
		"PIGO_TOOL_NAME":  name,
		"PIGO_SESSION_ID": t.session.meta.ID,
	}
	if len(args) <= maxArgsEnv {
		env["PIGO_TOOL_ARGS"] = string(args)
	}
	for k, v := range t.spec.env {
		env[k] = v
	}
	res, err := t.server.runInSession(ctx, t.session, ext.RunRequest{
		Command:  t.spec.Command,
		Stdin:    args,
		Env:      env,
		Timeout:  t.spec.timeout,
		OnOutput: progressReporter(onUpdate),
	})
	out := clipOutput(strings.TrimRight(res.Stdout, "\n"))
	if onUpdate != nil {
		onUpdate(textResult(out))
	}
	if reason := stopReason(ctx); reason != "" {
		return textResult(out), fmt.Errorf("%s: the turn was stopped (%s); the command was terminated\n%s", name, reason, out)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return textResult(out), fmt.Errorf("%s: command timed out after %s\n%s", name, t.spec.timeout, out)
	}
	if err != nil {
		return textResult(out), fmt.Errorf("%s: %v\n%s", name, err, out)
	}
	if res.Exit != 0 {
		text := out
		if tail := strings.TrimSpace(res.Stderr); tail != "" {
			text = strings.TrimSpace(text + "\n" + clipOutput(tail))
		}
		return agentcore.AgentToolResult{
			Content: agentcore.ContentList{agentcore.NewTextContent(text)},
			Details: map[string]any{"exitCode": res.Exit},
		}, fmt.Errorf("%s: command exited with code %d\n%s", name, res.Exit, text)
	}
	return agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent(out)},
		Details: map[string]any{"exitCode": 0},
	}, nil
}

// progressReporter streams a command's output tail to onUpdate a few times a
// second, so a long command shows progress instead of looking hung.
func progressReporter(onUpdate agentcore.ToolUpdateFunc) func([]byte) {
	if onUpdate == nil {
		return nil
	}
	var (
		tail     []byte
		lastSent time.Time
	)
	return func(p []byte) {
		tail = append(tail, p...)
		if len(tail) > 4096 {
			tail = tail[len(tail)-4096:]
		}
		if time.Since(lastSent) < 300*time.Millisecond {
			return
		}
		lastSent = time.Now()
		onUpdate(textResult(string(tail)))
	}
}

// clipOutput keeps a command's output within what a tool result carries.
func clipOutput(out string) string {
	if len(out) > 30_000 {
		return out[:12_000] + "\n[truncated]\n" + out[len(out)-12_000:]
	}
	return out
}

// configuredTool presents a Go extension's tool under the config's name, with
// the config's description and schema when it gives them.
type configuredTool struct {
	agentcore.AgentTool
	name        string
	description string
	schema      json.RawMessage
}

func (t *configuredTool) Name() string { return t.name }

func (t *configuredTool) Description() string {
	if t.description != "" {
		return t.description
	}
	return t.AgentTool.Description()
}

func (t *configuredTool) Schema() json.RawMessage {
	if t.schema != nil {
		return t.schema
	}
	return t.AgentTool.Schema()
}

func (t *configuredTool) Detail(args json.RawMessage) string {
	if d, ok := t.AgentTool.(ext.Detailer); ok {
		return d.Detail(args)
	}
	return ""
}

// applyExtensions puts the configured extensions into a session's tool set,
// in config order: a name already in the set is replaced where it stands, a new
// one is appended.
func (s *apiServer) applyExtensions(managed *managedSession, tools []agentcore.AgentTool) ([]agentcore.AgentTool, error) {
	if s.exts == nil {
		return tools, nil
	}
	for _, spec := range s.exts.tools {
		at := -1
		for i, t := range tools {
			if t.Name() == spec.Name {
				at = i
				break
			}
		}
		var builtin agentcore.AgentTool
		if at >= 0 {
			builtin = tools[at]
		}
		var tool agentcore.AgentTool
		if spec.Command != "" {
			tool = &extCommandTool{spec: spec, server: s, session: managed}
		} else {
			factory, _ := ext.Lookup(spec.Go) // checked at load
			built, err := factory(extSession{server: s, managed: managed}, builtin, spec.env)
			if err != nil {
				return nil, fmt.Errorf("tool %s (%s): %w", spec.Name, spec.Go, err)
			}
			if built == nil {
				return nil, fmt.Errorf("tool %s (%s): the extension built no tool", spec.Name, spec.Go)
			}
			tool = &configuredTool{AgentTool: built, name: spec.Name, description: strings.TrimSpace(spec.Description), schema: spec.schema}
		}
		if at >= 0 {
			tools[at] = tool
		} else {
			tools = append(tools, tool)
		}
	}
	return tools, nil
}

// toolDetail says what a call shows in the activity log: the config's detail
// template, then what the tool itself says (ext.Detailer), then the built-in
// rule for a built-in's name, then an extension's first string argument.
// tools is the session's tool set, or nil where it is not loaded.
func (s *apiServer) toolDetail(tools []agentcore.AgentTool, name string, args json.RawMessage) string {
	spec, isExt := s.exts.tool(name)
	if isExt && spec.Detail != "" {
		return clip(renderDetail(spec.Detail, args), detailLimit)
	}
	for _, t := range tools {
		if t.Name() != name {
			continue
		}
		if d, ok := t.(ext.Detailer); ok {
			if detail := strings.TrimSpace(d.Detail(args)); detail != "" {
				return clip(detail, detailLimit)
			}
		}
		break
	}
	if isExt && !spec.replaces {
		if first := firstStringArg(args); first != "" {
			return clip(first, detailLimit)
		}
	}
	return toolDetail(name, args)
}

var detailField = regexp.MustCompile(`\{([A-Za-z0-9_.-]+)\}`)

// renderDetail fills a "{field}" template from a call's arguments.
func renderDetail(template string, raw json.RawMessage) string {
	var args map[string]json.RawMessage
	_ = json.Unmarshal(raw, &args)
	return strings.TrimSpace(detailField.ReplaceAllStringFunc(template, func(m string) string {
		value, ok := args[m[1:len(m)-1]]
		if !ok {
			return ""
		}
		var s string
		if json.Unmarshal(value, &s) == nil {
			return firstLine(s)
		}
		return string(value)
	}))
}

// firstStringArg is the first string-valued argument, in the order the model
// wrote them.
func firstStringArg(raw json.RawMessage) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return ""
	}
	for dec.More() {
		if _, err := dec.Token(); err != nil { // the key
			return ""
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return ""
		}
		var s string
		if json.Unmarshal(value, &s) == nil && strings.TrimSpace(s) != "" {
			return firstLine(strings.TrimSpace(s))
		}
	}
	return ""
}

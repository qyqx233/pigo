// Package ext is where pigo-server's Go tool extensions register.
//
// An extension is a Go package that calls Register from init(); the server
// blank-imports ext/all, which imports every extension in this repository.
// Registering only makes an implementation available: the tools config file
// (-tools-config) enables it, under a name of the config's choosing:
//
//	tools:
//	  - name: webfetch          # the name the tool is known by
//	    go: webfetch_allowlist  # the registered implementation
//	    env:
//	      ALLOW_HOSTS: ${PIGO_EXT_ALLOW_HOSTS}
//
// Using a built-in tool's name replaces the built-in; the factory is handed
// the built-in it replaces, so an extension can change part of its behaviour
// and delegate the rest.
//
// Unlike the config file's command tools, which run inside the session's
// sandbox, a Go extension runs in the server process on the host: it is not
// confined by the sandbox, and keeping it safe is the extension's job. That is
// also what makes it the place for secrets the model must never see.
//
// See spec/tool-extensions.md.
package ext

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
)

// Session is the session a tool is built for.
type Session interface {
	ID() string
	UserID() string
	// Workspace is the session workspace's path on the host.
	Workspace() string
	// Run runs a command in the session's sandbox container — the one its
	// bash commands run in — starting the container if it is not running.
	Run(ctx context.Context, req RunRequest) (RunResult, error)
}

// RunRequest is one command to run in the session's sandbox.
type RunRequest struct {
	// Command is a shell command line, run by /bin/sh in /workspace.
	Command string
	// Stdin is the command's standard input.
	Stdin []byte
	// Env is added to this command's environment only.
	Env map[string]string
	// Timeout bounds the command; zero means two minutes.
	Timeout time.Duration
	// OnOutput, when set, receives standard output as it arrives.
	OnOutput func([]byte)
}

// RunResult is how a command ended.
type RunResult struct {
	Stdout string
	Stderr string
	Exit   int
}

// Factory builds the tool for one session. builtin is the built-in tool the
// configured name replaces, or nil when the name is new; env is the entry's
// env from the config, with its variables expanded.
//
// The tool's own Name is not what it is registered under: the server uses the
// config's name whatever the factory returns.
type Factory func(s Session, builtin agentcore.AgentTool, env map[string]string) (agentcore.AgentTool, error)

// Detailer is implemented by a tool that says what one of its calls shows in
// the activity log (a command line, a path, a query). Without it the server
// shows the call's first string argument.
type Detailer interface {
	Detail(args json.RawMessage) string
}

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register makes an implementation available under name. It is meant for
// init(); registering a name twice panics, as two extensions claiming one name
// is a build mistake.
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if name == "" || f == nil {
		panic("ext: Register needs a name and a factory")
	}
	if _, dup := factories[name]; dup {
		panic(fmt.Sprintf("ext: %q registered twice", name))
	}
	factories[name] = f
}

// Lookup returns the implementation registered under name.
func Lookup(name string) (Factory, bool) {
	mu.RLock()
	defer mu.RUnlock()
	f, ok := factories[name]
	return f, ok
}

// Names lists the registered implementations, sorted.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(factories))
	for name := range factories {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

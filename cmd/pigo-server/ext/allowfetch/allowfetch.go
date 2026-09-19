// Package allowfetch is an example Go tool extension: it wraps the built-in
// webfetch with a host allowlist. Calls to an allowed host are delegated to the
// built-in unchanged; any other host is refused before a request is made.
//
//	tools:
//	  - name: webfetch
//	    go: webfetch_allowlist
//	    env:
//	      ALLOW_HOSTS: docs.example.com,*.example.org
//
// A pattern "*.example.org" matches example.org's subdomains, not example.org
// itself.
package allowfetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/smallnest/pigo/cmd/pigo-server/ext"
	"github.com/smallnest/pigo/internal/agentcore"
)

// Name is what the config's go: field refers to.
const Name = "webfetch_allowlist"

func init() { ext.Register(Name, New) }

// New builds the tool. It must replace a tool (the built-in webfetch), and it
// needs a non-empty ALLOW_HOSTS.
func New(_ ext.Session, builtin agentcore.AgentTool, env map[string]string) (agentcore.AgentTool, error) {
	if builtin == nil {
		return nil, errors.New(Name + " wraps webfetch: configure it under the name webfetch")
	}
	var hosts []string
	for _, h := range strings.Split(env["ALLOW_HOSTS"], ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		return nil, errors.New(Name + " needs ALLOW_HOSTS (comma-separated hosts)")
	}
	return &tool{AgentTool: builtin, hosts: hosts}, nil
}

type tool struct {
	agentcore.AgentTool
	hosts []string
}

func (t *tool) Description() string {
	return t.AgentTool.Description() + "\n\nOnly these hosts can be fetched: " + strings.Join(t.hosts, ", ") + "."
}

func (t *tool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	var a struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return text("webfetch: invalid arguments"), fmt.Errorf("webfetch: invalid arguments: %w", err)
	}
	u, err := url.Parse(strings.TrimSpace(a.URL))
	if err != nil || u.Hostname() == "" {
		return text("webfetch: invalid url"), fmt.Errorf("webfetch: invalid url %q", a.URL)
	}
	if !t.allowed(strings.ToLower(u.Hostname())) {
		msg := fmt.Sprintf("webfetch: %s is not on this deployment's allowlist (%s)", u.Hostname(), strings.Join(t.hosts, ", "))
		return text(msg), errors.New(msg)
	}
	return t.AgentTool.Execute(ctx, id, args, onUpdate)
}

func (t *tool) Detail(args json.RawMessage) string {
	var a struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(args, &a)
	return a.URL
}

func (t *tool) allowed(host string) bool {
	for _, h := range t.hosts {
		if suffix, ok := strings.CutPrefix(h, "*."); ok {
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
		} else if host == h {
			return true
		}
	}
	return false
}

func text(s string) agentcore.AgentToolResult {
	return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent(s)}}
}

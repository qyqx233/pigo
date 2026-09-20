// The tools config file (-tools-config / PIGO_TOOLS_CONFIG): extension tools
// that add to or replace the built-ins, and the naming profiles that decide
// what the tools are called when sent to a given model. It is read once at
// startup and checked in full; any problem stops the server, since a tool or
// a rule that was configured and silently not applied is harder to notice
// than a server that will not start.
//
// See spec/tool-extensions.md.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/smallnest/pigo/cmd/pigo-server/ext"
	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/agenttool"
)

// hostToolNames are the built-in tools the server itself provides (hostTools),
// which an extension can replace and a naming profile can rename.
var hostToolNames = []string{"read", "write", "edit", "grep", "find", "bash", "todo", "webfetch", "websearch", "task"}

const (
	defaultExtTimeout = 2 * time.Minute
	maxExtTimeout     = 10 * time.Minute
)

var (
	// extToolNamePattern keeps an extension's name in the shape of the
	// built-ins' — lowercase, since -tools matching lowercases names.
	extToolNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	// wireNamePattern is what the model APIs accept as a tool name.
	wireNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// toolsConfigFile is the file as written.
type toolsConfigFile struct {
	Tools  []extToolSpec `yaml:"tools"`
	Naming struct {
		Profiles map[string]map[string]profileEntry `yaml:"profiles"`
		Rules    []namingRule                       `yaml:"rules"`
	} `yaml:"naming"`
}

// extToolSpec is one entry of tools:, a command tool or a Go tool.
type extToolSpec struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	Schema      any               `yaml:"schema"`
	Command     string            `yaml:"command"`
	Go          string            `yaml:"go"`
	Timeout     string            `yaml:"timeout"`
	Detail      string            `yaml:"detail"`
	Env         map[string]string `yaml:"env"`
	// Scope is "all" (the default: every session has the tool) or "scene":
	// only the scenes that list it get it (see scenes.go).
	Scope string `yaml:"scope"`

	// Resolved at load.
	schema   json.RawMessage // nil: keep the implementation's
	timeout  time.Duration
	env      map[string]string // expanded
	replaces bool              // the name is a built-in's
}

// source labels the entry for reports.
func (t *extToolSpec) source() string {
	kind := "command"
	if t.Go != "" {
		kind = "go"
	}
	if t.replaces {
		kind += "-override"
	}
	if t.Scope == "scene" {
		kind += "-scene"
	}
	return kind
}

// profileEntry is how a profile renames one tool: a bare name, or
// {name, description}.
type profileEntry struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

func (p *profileEntry) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		p.Name = node.Value
		return nil
	}
	type plain profileEntry
	var v plain
	if err := node.Decode(&v); err != nil {
		return err
	}
	*p = profileEntry(v)
	return nil
}

// namingRule picks a profile for the calls whose provider and model match its
// patterns: globs where * matches anything (model ids contain "/", so path
// globs will not do) and ? one character, case-insensitive; an empty pattern
// matches everything.
type namingRule struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	Profile  string `yaml:"profile"`

	provider, model *regexp.Regexp
}

func (r namingRule) matches(providerName, model string) bool {
	return (r.provider == nil || r.provider.MatchString(providerName)) &&
		(r.model == nil || r.model.MatchString(model))
}

// compileGlob turns a rule pattern into a regexp; nil for an empty pattern.
func compileGlob(pattern string) *regexp.Regexp {
	if pattern == "" {
		return nil
	}
	var b strings.Builder
	b.WriteString("(?i)^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// namingProfile is a validated profile: canonical name to the entry, and back.
type namingProfile struct {
	name    string
	forward map[string]profileEntry
	reverse map[string]string // model-facing name → canonical
}

// wire is the name the model sees for a canonical tool name.
func (p *namingProfile) wire(name string) string {
	if e, ok := p.forward[name]; ok && e.Name != "" {
		return e.Name
	}
	return name
}

// canonical is the tool a model-facing name refers to.
func (p *namingProfile) canonical(name string) string {
	if c, ok := p.reverse[name]; ok {
		return c
	}
	return name
}

// toolExtensions is the loaded config. The zero value (no file) adds nothing
// and renames nothing.
type toolExtensions struct {
	tools    []*extToolSpec
	byName   map[string]*extToolSpec
	profiles map[string]*namingProfile
	rules    []namingRule
}

// tool returns the extension configured under a name.
func (x *toolExtensions) tool(name string) (*extToolSpec, bool) {
	if x == nil {
		return nil, false
	}
	t, ok := x.byName[name]
	return t, ok
}

// names lists the extension tools that are not built-ins, in config order.
func (x *toolExtensions) addedNames() []string {
	if x == nil {
		return nil
	}
	var out []string
	for _, t := range x.tools {
		// A scene tool is not in a session's own set.
		if !t.replaces && t.Scope != "scene" {
			out = append(out, t.Name)
		}
	}
	return out
}

// hasCommandTools reports whether any extension runs in the sandbox.
func (x *toolExtensions) hasCommandTools() bool {
	if x == nil {
		return false
	}
	for _, t := range x.tools {
		if t.Command != "" {
			return true
		}
	}
	return false
}

// profileFor returns the profile of the first rule matching the call, or nil.
func (x *toolExtensions) profileFor(providerName, model string) *namingProfile {
	if x == nil {
		return nil
	}
	for _, r := range x.rules {
		if r.matches(providerName, model) {
			return x.profiles[r.Profile]
		}
	}
	return nil
}

// extToolReport and namingRuleReport are what /healthz shows of the config.
type extToolReport struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

type namingRuleReport struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Profile  string `json:"profile"`
}

func (x *toolExtensions) report() map[string]any {
	tools := []extToolReport{}
	rules := []namingRuleReport{}
	if x != nil {
		for _, t := range x.tools {
			tools = append(tools, extToolReport{Name: t.Name, Source: t.source()})
		}
		for _, r := range x.rules {
			rules = append(rules, namingRuleReport{Provider: r.Provider, Model: r.Model, Profile: r.Profile})
		}
	}
	return map[string]any{"tools": tools, "naming": rules}
}

// loadToolsConfig reads and checks the file; an empty path loads nothing.
func loadToolsConfig(file string) (*toolExtensions, error) {
	if strings.TrimSpace(file) == "" {
		return &toolExtensions{}, nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("tools config: %w", err)
	}
	x, err := parseToolsConfig(data, os.LookupEnv)
	if err != nil {
		return nil, fmt.Errorf("tools config %s: %w", file, err)
	}
	return x, nil
}

// parseToolsConfig decodes and validates a config; lookupEnv resolves the
// ${VAR} references in env values.
func parseToolsConfig(data []byte, lookupEnv func(string) (string, bool)) (*toolExtensions, error) {
	var file toolsConfigFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)                                                // a misspelt key is an error, not a silent no-op
	if err := dec.Decode(&file); err != nil && !errors.Is(err, io.EOF) { // io.EOF: an empty file
		return nil, err
	}

	x := &toolExtensions{byName: map[string]*extToolSpec{}, profiles: map[string]*namingProfile{}}
	builtin := map[string]bool{}
	for _, name := range hostToolNames {
		builtin[name] = true
	}
	for i := range file.Tools {
		t := &file.Tools[i]
		if err := resolveExtTool(t, builtin, lookupEnv); err != nil {
			label := t.Name
			if label == "" {
				label = fmt.Sprintf("#%d", i+1)
			}
			return nil, fmt.Errorf("tools[%s]: %w", label, err)
		}
		if _, dup := x.byName[t.Name]; dup {
			return nil, fmt.Errorf("tools[%s]: configured twice", t.Name)
		}
		x.byName[t.Name] = t
		x.tools = append(x.tools, t)
	}

	// Every tool that can be offered: the built-ins and the extensions.
	known := map[string]bool{}
	for name := range builtin {
		known[name] = true
	}
	for _, t := range x.tools {
		known[t.Name] = true
	}
	for name, entries := range file.Naming.Profiles {
		p, err := resolveProfile(name, entries, known)
		if err != nil {
			return nil, err
		}
		x.profiles[name] = p
	}
	for i, r := range file.Naming.Rules {
		if _, ok := x.profiles[r.Profile]; !ok {
			return nil, fmt.Errorf("naming.rules[%d]: no profile %q", i+1, r.Profile)
		}
		r.provider, r.model = compileGlob(r.Provider), compileGlob(r.Model)
		x.rules = append(x.rules, r)
	}
	return x, nil
}

func resolveExtTool(t *extToolSpec, builtin map[string]bool, lookupEnv func(string) (string, bool)) error {
	t.Name = strings.TrimSpace(t.Name)
	if !extToolNamePattern.MatchString(t.Name) {
		return errors.New("name must be lowercase letters, digits and underscores, starting with a letter")
	}
	t.replaces = builtin[t.Name]
	switch t.Scope = strings.ToLower(strings.TrimSpace(t.Scope)); t.Scope {
	case "", "all":
		t.Scope = "all"
	case "scene":
		if t.replaces {
			return errors.New("scope: scene cannot replace a built-in: a built-in is in every session")
		}
	default:
		return fmt.Errorf("scope %q: use all or scene", t.Scope)
	}
	t.Command, t.Go = strings.TrimSpace(t.Command), strings.TrimSpace(t.Go)
	switch {
	case t.Command == "" && t.Go == "":
		return errors.New("needs command: or go:")
	case t.Command != "" && t.Go != "":
		return errors.New("has both command: and go:; pick one")
	}
	if t.Go != "" {
		if _, ok := ext.Lookup(t.Go); !ok {
			return fmt.Errorf("no Go extension %q is compiled in (have: %s)", t.Go, strings.Join(ext.Names(), ", "))
		}
		if t.Timeout != "" {
			return errors.New("timeout: applies to command tools only")
		}
	}
	if t.Command != "" {
		// A command tool is all the model knows of it: a new one, or one
		// replacing a built-in, says what it does and takes.
		if strings.TrimSpace(t.Description) == "" {
			return errors.New("a command tool needs a description")
		}
		if t.replaces && t.Schema == nil {
			return errors.New("a command tool replacing a built-in needs its own schema")
		}
		t.timeout = defaultExtTimeout
		if t.Timeout != "" {
			d, err := time.ParseDuration(t.Timeout)
			if err != nil || d <= 0 {
				return fmt.Errorf("timeout %q is not a duration", t.Timeout)
			}
			if d > maxExtTimeout {
				return fmt.Errorf("timeout %s exceeds the %s limit", d, maxExtTimeout)
			}
			t.timeout = d
		}
		if t.Schema == nil {
			t.Schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
	}
	if t.Schema != nil {
		raw, err := json.Marshal(t.Schema)
		if err != nil {
			return fmt.Errorf("schema: %w", err)
		}
		// The registry drops a tool whose schema does not compile, silently;
		// catch it here instead.
		if err := agenttool.NewToolRegistry().Register(schemaProbe{name: t.Name, schema: raw}); err != nil {
			return fmt.Errorf("schema: %w", err)
		}
		t.schema = raw
	}
	t.env = map[string]string{}
	for key, value := range t.Env {
		if key == "" || strings.ContainsAny(key, "= ") {
			return fmt.Errorf("env: bad variable name %q", key)
		}
		var missing []string
		expanded := os.Expand(value, func(name string) string {
			v, ok := lookupEnv(name)
			if !ok {
				missing = append(missing, name)
			}
			return v
		})
		if len(missing) > 0 {
			return fmt.Errorf("env %s: the server's environment has no %s", key, strings.Join(missing, ", "))
		}
		t.env[key] = expanded
	}
	return nil
}

func resolveProfile(name string, entries map[string]profileEntry, known map[string]bool) (*namingProfile, error) {
	p := &namingProfile{name: name, forward: map[string]profileEntry{}, reverse: map[string]string{}}
	for tool, e := range entries {
		if !known[tool] {
			return nil, fmt.Errorf("naming.profiles.%s: no tool %q", name, tool)
		}
		if e.Name != "" && !wireNamePattern.MatchString(e.Name) {
			return nil, fmt.Errorf("naming.profiles.%s.%s: %q is not a valid tool name for model APIs", name, tool, e.Name)
		}
		p.forward[tool] = e
	}
	// Every tool's model-facing name must be distinct — a renamed tool must
	// not land on another tool's name, renamed or not.
	owner := map[string]string{}
	for tool := range known {
		wire := p.wire(tool)
		if other, taken := owner[wire]; taken {
			return nil, fmt.Errorf("naming.profiles.%s: %s and %s would both be called %q", name, other, tool, wire)
		}
		owner[wire] = tool
		if wire != tool {
			p.reverse[wire] = tool
		}
	}
	return p, nil
}

// schemaProbe lets a schema be compiled by the registry on its own.
type schemaProbe struct {
	agentcore.AgentTool
	name   string
	schema json.RawMessage
}

func (p schemaProbe) Name() string            { return p.name }
func (p schemaProbe) Schema() json.RawMessage { return p.schema }

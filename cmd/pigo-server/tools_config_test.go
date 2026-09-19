package main

import (
	"strings"
	"testing"
	"time"
)

func testEnv(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

const fullToolsConfig = `
tools:
  - name: xlsx_summary
    description: Summarize a workbook.
    schema:
      type: object
      properties:
        path: { type: string }
      required: [path]
    command: python3 /opt/tools/xlsx.py
    timeout: 30s
    detail: "{path}"
  - name: websearch
    description: Search the company index.
    schema: { type: object, properties: { query: { type: string } }, required: [query] }
    command: /opt/tools/search
    env:
      TOKEN: "Bearer ${SEARCH_TOKEN}"
  - name: webfetch
    go: webfetch_allowlist
    env:
      ALLOW_HOSTS: example.com
naming:
  profiles:
    claude:
      bash: Bash
      read: Read
      websearch: { name: WebSearch, description: Search the web. }
    snake:
      websearch: web_search
  rules:
    - model: "*Claude*"
      profile: claude
    - provider: codebuddy-proxy
      profile: snake
`

func TestParseToolsConfig(t *testing.T) {
	x, err := parseToolsConfig([]byte(fullToolsConfig), testEnv(map[string]string{"SEARCH_TOKEN": "s3cret"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(x.tools) != 3 {
		t.Fatalf("tools = %d", len(x.tools))
	}
	xlsx, _ := x.tool("xlsx_summary")
	if xlsx.replaces || xlsx.timeout != 30*time.Second || !strings.Contains(string(xlsx.schema), `"path"`) || xlsx.source() != "command" {
		t.Errorf("xlsx_summary = %+v", xlsx)
	}
	search, _ := x.tool("websearch")
	if !search.replaces || search.timeout != defaultExtTimeout || search.env["TOKEN"] != "Bearer s3cret" || search.source() != "command-override" {
		t.Errorf("websearch = %+v", search)
	}
	fetch, _ := x.tool("webfetch")
	if !fetch.replaces || fetch.schema != nil || fetch.source() != "go-override" {
		t.Errorf("webfetch = %+v", fetch)
	}
	if got := x.addedNames(); len(got) != 1 || got[0] != "xlsx_summary" {
		t.Errorf("addedNames = %v", got)
	}

	// Rules: first match wins, patterns ignore case, no match is no profile.
	if p := x.profileFor("openrouter", "anthropic/claude-sonnet-5"); p == nil || p.name != "claude" {
		t.Errorf("claude model got %v", p)
	}
	if p := x.profileFor("codebuddy-proxy", "global:hy3"); p == nil || p.name != "snake" {
		t.Errorf("codebuddy got %v", p)
	}
	if p := x.profileFor("deepseek", "deepseek-v4-flash"); p != nil {
		t.Errorf("unmatched model got profile %s", p.name)
	}
	claude := x.profiles["claude"]
	if claude.wire("bash") != "Bash" || claude.wire("grep") != "grep" || claude.canonical("WebSearch") != "websearch" || claude.canonical("grep") != "grep" {
		t.Errorf("claude profile = %+v", claude)
	}
}

func TestParseToolsConfigEmpty(t *testing.T) {
	for _, doc := range []string{"", "# nothing\n"} {
		x, err := parseToolsConfig([]byte(doc), testEnv(nil))
		if err != nil || len(x.tools) != 0 || x.profileFor("any", "model") != nil {
			t.Errorf("%q: %+v %v", doc, x, err)
		}
	}
	x, err := loadToolsConfig("")
	if err != nil || x == nil {
		t.Errorf("no file: %v %v", x, err)
	}
}

func TestParseToolsConfigRefuses(t *testing.T) {
	cases := []struct {
		name, doc, want string
	}{
		{"unknown key", "tools:\n  - name: a\n    comand: x\n", "comand"},
		{"no implementation", "tools:\n  - name: a\n    description: d\n", "needs command: or go:"},
		{"both", "tools:\n  - name: a\n    description: d\n    command: x\n    go: webfetch_allowlist\n", "pick one"},
		{"bad name", "tools:\n  - name: WebThing\n    description: d\n    command: x\n", "lowercase"},
		{"unknown go", "tools:\n  - name: a\n    go: nope\n", `no Go extension "nope"`},
		{"no description", "tools:\n  - name: a\n    command: x\n", "needs a description"},
		{"override without schema", "tools:\n  - name: bash\n    description: d\n    command: x\n", "needs its own schema"},
		{"bad schema", "tools:\n  - name: a\n    description: d\n    command: x\n    schema: { type: 7 }\n", "schema"},
		{"long timeout", "tools:\n  - name: a\n    description: d\n    command: x\n    timeout: 11m\n", "exceeds"},
		{"go timeout", "tools:\n  - name: webfetch\n    go: webfetch_allowlist\n    timeout: 1m\n", "command tools only"},
		{"missing env", "tools:\n  - name: a\n    description: d\n    command: x\n    env: { K: \"${NOPE}\" }\n", "has no NOPE"},
		{"twice", "tools:\n  - name: a\n    description: d\n    command: x\n  - name: a\n    description: d\n    command: y\n", "configured twice"},
		{"profile unknown tool", "naming:\n  profiles:\n    p: { nope: Nope }\n", `no tool "nope"`},
		{"invalid wire name", "naming:\n  profiles:\n    p: { bash: \"Run Bash\" }\n", "not a valid tool name"},
		{"collision", "naming:\n  profiles:\n    p: { read: grep }\n", `both be called "grep"`},
		{"rule without profile", "naming:\n  rules:\n    - model: x\n      profile: p\n", `no profile "p"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseToolsConfig([]byte(tc.doc), testEnv(nil))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

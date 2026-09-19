// The activity log: what a turn did, one line per step, for the page to show
// as a scrolling log above the answer — the model's short narration between
// tool calls, and each tool call with what it ran, whether it worked, how long
// it took, and (while a command runs) the tail of its output.
//
// The same summaries serve the live stream and history, so a turn looks the
// same while it runs and after a reload.
package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/smallnest/pigo/internal/agentcore"
)

// activityItem is one line of the log.
type activityItem struct {
	// Kind is "text" (the model's narration) or "tool".
	Kind string `json:"kind"`
	// Text is the narration, or a failed tool's summary.
	Text   string `json:"text,omitempty"`
	Tool   string `json:"tool,omitempty"`
	ID     string `json:"id,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Status is "running", "ok" or "error".
	Status    string `json:"status,omitempty"`
	ElapsedMs int64  `json:"elapsedMs,omitempty"`
	// Output is the tail of a running command's output.
	Output string `json:"output,omitempty"`
}

const (
	detailLimit  = 160
	summaryLimit = 200
	outputLines  = 3
	outputLimit  = 600
)

// toolDetail says what a tool call is doing, in one short line: the command's
// first line, the file, the URL, the query.
func toolDetail(name string, raw json.RawMessage) string {
	var args map[string]any
	if json.Unmarshal(raw, &args) != nil {
		return clip(strings.TrimSpace(string(raw)), detailLimit)
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := args[k].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	var detail string
	switch name {
	case "bash":
		cmd := str("command")
		detail = firstLine(cmd)
		if strings.Contains(cmd, "\n") {
			detail += " …"
		}
	case "read", "write", "edit":
		detail = str("path", "file_path", "file")
	case "grep":
		detail = str("pattern")
		if p := str("path", "glob"); p != "" {
			detail += "  " + p
		}
	case "find":
		detail = str("pattern", "name", "path")
	case "webfetch":
		detail = str("url")
	case "websearch":
		detail = str("query")
	case "todo":
		if todos, ok := args["todos"].([]any); ok {
			done := 0
			for _, t := range todos {
				if m, ok := t.(map[string]any); ok && m["status"] == "completed" {
					done++
				}
			}
			detail = fmt.Sprintf("%d/%d 完成", done, len(todos))
		}
	}
	if detail == "" {
		compact, _ := json.Marshal(args)
		detail = string(compact)
	}
	return clip(detail, detailLimit)
}

var exitCodePattern = regexp.MustCompile(`command exited with code (\d+)`)

// toolFailure is the line shown under a failed tool call: its exit code if it
// had one, and the last line of what it printed — where an error usually is.
// A successful call needs no summary.
func toolFailure(text string, isError bool) string {
	if !isError {
		return ""
	}
	lines := nonEmptyLines(text)
	if len(lines) == 0 {
		return "失败"
	}
	last := lines[len(lines)-1]
	if m := exitCodePattern.FindStringSubmatch(text); m != nil {
		if len(lines) == 1 {
			return "exit " + m[1]
		}
		return clip("exit "+m[1]+" · "+last, summaryLimit)
	}
	return clip(last, summaryLimit)
}

// outputTail is the last few lines of a running command's output.
func outputTail(text string) string {
	lines := nonEmptyLines(text)
	if len(lines) > outputLines {
		lines = lines[len(lines)-outputLines:]
	}
	out := []rune(strings.Join(lines, "\n"))
	if len(out) > outputLimit {
		return "…" + string(out[len(out)-outputLimit:])
	}
	return string(out)
}

func resultText(r agentcore.AgentToolResult) string {
	return agentcore.ContentToText(r.Content)
}

func nonEmptyLines(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimRight(line, " \t\r"); strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func clip(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	r := []rune(s)
	return string(r[:limit]) + "…"
}

// truncateMiddle keeps the head and tail of a long text, for a tool's output in
// history: enough to see how it began and ended without shipping megabytes.
func truncateMiddle(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	half := limit / 2
	return string(r[:half]) + fmt.Sprintf("\n… 省略 %d 个字符 …\n", len(r)-2*half) + string(r[len(r)-half:])
}

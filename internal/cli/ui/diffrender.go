// This file holds the shared colored-diff renderer used wherever a tool result
// carries a unified diff (the edit tool today): the compact REPL/btw/goal
// output goes through RenderDiffLine, and the TUI reuses DiffFromDetails to
// pull the diff out of a result's Details metadata. Coloring follows the
// convention of git and Claude Code - red removals, green additions, dim
// context and file headers, cyan hunk markers - gated by the same NO_COLOR /
// non-terminal rules as every other ui color.
package ui

import (
	"fmt"
	"strings"
)

// MaxDiffPreviewLines caps how many diff lines the compact tool-result
// renderer prints before eliding the rest with a notice. Without it a large
// replace_all would scroll pages of diff through the REPL.
const MaxDiffPreviewLines = 50

// DiffFromDetails extracts the unified diff a tool reported in its Details
// metadata (the edit tool stores map[string]any{"diff": ...}). The boolean is
// false - and the string empty - when there is no diff to show, so callers can
// fall back to the generic rendering. Any tool that starts reporting a "diff"
// key gets colored output for free.
func DiffFromDetails(details any) (string, bool) {
	m, ok := details.(map[string]any)
	if !ok {
		return "", false
	}
	diff, ok := m["diff"].(string)
	if !ok || diff == "" {
		return "", false
	}
	return diff, true
}

// RenderDiffLine renders one line of a unified diff with ANSI colors:
// ---/+++ file headers dim, @@ hunk markers cyan, removals red, additions
// green, unchanged context dim, and elision notices (leading ellipsis) dim.
// Anything else (including empty lines) passes through unchanged. enabled
// gates the codes as usual.
func RenderDiffLine(line string, enabled bool) string {
	switch {
	case strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ "):
		return Colorize(enabled, Dim, line)
	case strings.HasPrefix(line, "@@"):
		return Colorize(enabled, Cyan, line)
	case strings.HasPrefix(line, "-"):
		return Colorize(enabled, Red, line)
	case strings.HasPrefix(line, "+"):
		return Colorize(enabled, Green, line)
	case strings.HasPrefix(line, " ") || strings.HasPrefix(line, "…"):
		return Colorize(enabled, Dim, line)
	default:
		return line
	}
}

// DiffPreviewLines splits a unified diff into at most MaxDiffPreviewLines
// lines, appending a "… (+N lines elided)" notice when more were dropped, so
// the compact renderer never scrolls pages of diff through the REPL.
func DiffPreviewLines(diff string) []string {
	lines := strings.Split(strings.TrimRight(diff, "\n"), "\n")
	if len(lines) <= MaxDiffPreviewLines {
		return lines
	}
	hidden := len(lines) - MaxDiffPreviewLines
	notice := fmt.Sprintf("… (+%d lines elided)", hidden)
	return append(lines[:MaxDiffPreviewLines:MaxDiffPreviewLines], notice)
}

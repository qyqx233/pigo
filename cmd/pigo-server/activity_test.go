package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolDetail(t *testing.T) {
	cases := []struct {
		name, args, want string
	}{
		{"bash", `{"command":"cd /workspace && ls -la"}`, "cd /workspace && ls -la"},
		{"bash", `{"command":"echo a\necho b"}`, "echo a …"},
		{"edit", `{"path":"tools/build.py","old_string":"x","new_string":"y"}`, "tools/build.py"},
		{"grep", `{"pattern":"TODO","path":"src"}`, "TODO  src"},
		{"webfetch", `{"url":"https://example.com"}`, "https://example.com"},
		{"todo", `{"todos":[{"content":"a","status":"completed"},{"content":"b","status":"pending"}]}`, "1/2 完成"},
		{"custom", `{"a":1}`, `{"a":1}`},
	}
	for _, tc := range cases {
		if got := toolDetail(tc.name, json.RawMessage(tc.args)); got != tc.want {
			t.Errorf("%s %s = %q, want %q", tc.name, tc.args, got, tc.want)
		}
	}
	long := `{"command":"` + strings.Repeat("x", 500) + `"}`
	if got := toolDetail("bash", json.RawMessage(long)); len([]rune(got)) > detailLimit+1 {
		t.Errorf("detail not clipped: %d", len(got))
	}
}

func TestToolFailure(t *testing.T) {
	if got := toolFailure("all good", false); got != "" {
		t.Errorf("success summary = %q", got)
	}
	out := "tool \"bash\" failed: bash: command exited with code 1\nrunning tests\nAssertionError: got 45 want 50\n"
	if got := toolFailure(out, true); got != "exit 1 · AssertionError: got 45 want 50" {
		t.Errorf("failure = %q", got)
	}
	if got := toolFailure(`unknown tool "x"`, true); got != `unknown tool "x"` {
		t.Errorf("rejected = %q", got)
	}
}

func TestOutputTail(t *testing.T) {
	if got := outputTail("a\n\nb\nc\nd\n"); got != "b\nc\nd" {
		t.Errorf("tail = %q", got)
	}
	if got := outputTail(strings.Repeat("长", 1000)); !strings.HasPrefix(got, "…") || len([]rune(got)) != outputLimit+1 {
		t.Errorf("long tail = %d runes", len([]rune(got)))
	}
}

// TestActivityFold: narration before a call moves into the log, output shows
// while the call runs and clears when it ends, and a rejected call with no
// start still gets a line.
func TestActivityFold(t *testing.T) {
	run := newTurnRun("s", turnLimits{})
	run.publish(streamEvent{Type: "delta", Text: "先看看目录。"})
	run.publish(streamEvent{Type: "tool", Phase: "start", Tool: "bash", ID: "c1", Detail: "ls"})
	run.publish(streamEvent{Type: "tool", Phase: "output", ID: "c1", Text: "a.txt"})
	snap, _, _ := run.subscribe()
	if snap.Text != "" || len(snap.Activity) != 2 || snap.Activity[0].Text != "先看看目录。" ||
		snap.Activity[1].Status != "running" || snap.Activity[1].Output != "a.txt" {
		t.Fatalf("mid-call = %+v", snap)
	}
	run.publish(streamEvent{Type: "tool", Phase: "end", ID: "c1", ElapsedMs: 120})
	run.publish(streamEvent{Type: "tool", Phase: "end", Tool: "nosuchtool", ID: "c2", IsError: true, Text: "unknown tool"})
	run.publish(streamEvent{Type: "delta", Text: "完成。"})
	snap, _, _ = run.subscribe()
	if snap.Text != "完成。" || len(snap.Activity) != 3 {
		t.Fatalf("after = %+v", snap)
	}
	if a := snap.Activity[1]; a.Status != "ok" || a.Output != "" || a.ElapsedMs != 120 {
		t.Errorf("ended call = %+v", a)
	}
	if a := snap.Activity[2]; a.Tool != "nosuchtool" || a.Status != "error" || a.Text != "unknown tool" {
		t.Errorf("rejected call = %+v", a)
	}
}

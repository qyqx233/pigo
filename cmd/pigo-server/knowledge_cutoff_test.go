package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/runtime"
)

func TestCutoffNote(t *testing.T) {
	configured := cutoffNote("2026-05")
	if !strings.HasPrefix(configured, "- Knowledge cutoff: 2026-05.") {
		t.Errorf("configured note = %q", configured)
	}
	vague := cutoffNote("  ")
	if strings.Contains(vague, "Knowledge cutoff:") {
		t.Errorf("unconfigured note names a date: %q", vague)
	}
	// Both forms must carry the reasoning, not just the fact: no harness
	// surveyed states a cutoff except Claude Code, so nothing in a third-party
	// model's training connects the date to what to do about it.
	for _, note := range []string{configured, vague} {
		if !strings.Contains(note, "does not exist") {
			t.Errorf("note lacks the inference it is there to block: %q", note)
		}
	}
}

// TestWithKnowledgeCutoffAnchor builds a real prompt and checks the note lands
// inside the environment block, on the line after the date. The anchor result
// is asserted so that a change to the upstream block fails here rather than
// silently moving the note to the end of the prompt.
func TestWithKnowledgeCutoffAnchor(t *testing.T) {
	ts := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	ws := t.TempDir()
	prompt, err := runtime.BuildSystemPrompt(runtime.PromptConfig{
		WorkingDir: ws, Root: ws, Now: func() time.Time { return ts },
	})
	if err != nil {
		t.Fatal(err)
	}
	out, ok := withKnowledgeCutoff(prompt, ts, "2026-05")
	if !ok {
		t.Fatal("the date anchor was not found; the upstream environment block changed")
	}
	if !strings.Contains(out, "- Date: 2026-09-20\n- Knowledge cutoff: 2026-05.") {
		t.Errorf("the note is not on the line after the date:\n%s", out)
	}
}

func TestWithKnowledgeCutoffFallback(t *testing.T) {
	out, ok := withKnowledgeCutoff("no environment block here", time.Now(), "")
	if ok {
		t.Error("reported an anchor in a prompt that has none")
	}
	if !strings.HasSuffix(out, cutoffNote("")) {
		t.Errorf("the note was not appended:\n%s", out)
	}
}

func TestNormalizeCutoff(t *testing.T) {
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		in, want string
		bad      bool
	}{
		{in: "", want: ""},
		{in: " 2026-05 ", want: "2026-05"},
		{in: "2026-05-31", want: "2026-05"},
		{in: "2026-09", want: "2026-09"},
		{in: "2026-10", bad: true},  // after today
		{in: "2027", bad: true},     // not a month
		{in: "May 2026", bad: true}, // not a date
	} {
		got, err := normalizeCutoff(c.in, now)
		if c.bad {
			if err == nil {
				t.Errorf("normalizeCutoff(%q) = %q, want an error", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("normalizeCutoff(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

// TestModelParamCutoffOnly covers a row that sets only the cutoff: the window
// and the threshold stay on the deployment default.
func TestModelParamCutoffOnly(t *testing.T) {
	got, err := modelParam{Provider: "FakeLLM", Model: "m", KnowledgeCutoff: "2026-05-31"}.validate()
	if err != nil {
		t.Fatalf("cutoff-only row rejected: %v", err)
	}
	if got.Provider != "fakellm" || got.KnowledgeCutoff != "2026-05" {
		t.Errorf("validate = %+v", got)
	}
	if _, err := (modelParam{Provider: "p", Model: "m"}).validate(); err == nil {
		t.Error("an empty row was accepted")
	}
}

// TestSessionPromptStatesCutoff is the end-to-end check: what the model
// actually receives carries the configured cutoff, and switching the session's
// model swaps in the new model's — and nothing else.
func TestSessionPromptStatesCutoff(t *testing.T) {
	server, llm := sceneServer(t)
	if err := server.settings.putModelParam(modelParam{Provider: "fakellm", Model: "fake-model", KnowledgeCutoff: "2026-05"}); err != nil {
		t.Fatal(err)
	}
	if err := server.customModels.add(customModel{ID: "fake-model-2", Label: "Fake 2", Provider: "fakellm", Scope: "public"}); err != nil {
		t.Fatal(err)
	}
	if err := server.settings.putModelParam(modelParam{Provider: "fakellm", Model: "fake-model-2", KnowledgeCutoff: "2025-08"}); err != nil {
		t.Fatal(err)
	}

	managed := createPlainSession(t, server)
	sendPrompt(t, server, managed, "hi", false)
	today := time.Now().Format("2006-01-02")
	if got := llm.last(t).system; !strings.Contains(got, "- Date: "+today+"\n- Knowledge cutoff: 2026-05.") {
		t.Errorf("the prompt does not state the cutoff on the line after the date:\n%s", got)
	}

	// Switching model updates the cutoff: it belongs to the model. Nothing else
	// in the prompt moves — an AGENTS.md written meanwhile (the model itself can
	// write in this workspace) must not slip in, any more than a scene edit
	// reaches a session that already snapshotted it.
	if err := os.WriteFile(managed.paths.Workspace+"/AGENTS.md", []byte("偷偷加的项目指令"), 0o644); err != nil {
		t.Fatal(err)
	}
	managed.mu.Lock()
	managed.meta.Model = "fake-model-2"
	err := server.applyHostConfig(managed)
	managed.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	sendPrompt(t, server, managed, "again", false)
	got := llm.last(t).system
	if !strings.Contains(got, "- Knowledge cutoff: 2025-08.") || strings.Contains(got, "2026-05") {
		t.Errorf("the prompt kept the previous model's cutoff:\n%s", got)
	}
	if strings.Contains(got, "偷偷加的项目指令") {
		t.Errorf("switching model rebuilt the prompt and picked up a new AGENTS.md:\n%s", got)
	}
}

// TestSessionPromptWithoutCutoff covers the default deployment: no model has a
// cutoff configured, and the prompt still tells the model its knowledge is old.
func TestSessionPromptWithoutCutoff(t *testing.T) {
	server, llm := sceneServer(t)
	managed := createPlainSession(t, server)
	sendPrompt(t, server, managed, "hi", false)
	got := llm.last(t).system
	if !strings.Contains(got, "- Your training data ends well before today's date above.") {
		t.Errorf("the prompt lacks the unconfigured note:\n%s", got)
	}
	if strings.Contains(got, "Knowledge cutoff:") {
		t.Errorf("the prompt names a cutoff nobody configured:\n%s", got)
	}
}

// TestSubagentPromptStatesCutoff checks the sub-agent gets its own environment:
// a sub-agent runs on a model of its own and has the same web tools, so it
// needs the date and the knowledge-cutoff note as much as the session does.
// Its prompt otherwise shares nothing with the session's.
func TestSubagentPromptStatesCutoff(t *testing.T) {
	server, llm, managed := taskServer(t)
	requireSandbox(t, server)
	if err := server.settings.putModelParam(modelParam{Provider: "fakellm", Model: "fake-model", KnowledgeCutoff: "2026-05"}); err != nil {
		t.Fatal(err)
	}
	sendPrompt(t, server, managed, "去查一下版本", false)
	llm.mu.Lock()
	system := llm.childSystem
	llm.mu.Unlock()
	if !strings.Contains(system, "- Date: "+time.Now().Format("2006-01-02")) {
		t.Errorf("the sub-agent has no date:\n%s", system)
	}
	if !strings.Contains(system, "- Knowledge cutoff: 2026-05.") {
		t.Errorf("the sub-agent has no cutoff note:\n%s", system)
	}
}

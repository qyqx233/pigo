// The knowledge-cutoff note: the line in the system prompt that tells a model
// its training data ends before today, so that meeting an unfamiliar name is
// evidence of its own staleness rather than evidence the thing does not exist.
//
// A model that skips this step answers about the world it was trained on and
// searches for the terms it already knows, which is how a real session decided
// a model released last month did not exist and rewrote the user's query until
// the search agreed (spec/prompt-tuning.md, case C1).
//
// Of the harnesses surveyed only Claude Code states a cutoff at all; pi and
// codex state none. The wording here therefore carries its own reasoning
// instead of assuming the model was trained to do it.
package main

import (
	"strings"
	"time"
)

// cutoffNote is the environment-block line for a model whose cutoff is
// configured, or the vague form when it is not. The vague form still ships: the
// reasoning to interrupt is "I do not recognize it, so it does not exist", and
// that does not need an exact date — while most proxied models have no
// published cutoff to configure, so requiring one would leave nearly every
// session with nothing.
func cutoffNote(cutoff string) string {
	const tail = " Anything released since is unfamiliar to you: not recognizing a name means your " +
		"knowledge is stale, not that the thing does not exist — check before you assert."
	if cutoff = strings.TrimSpace(cutoff); cutoff != "" {
		return "- Knowledge cutoff: " + cutoff + ". Your training data ends there, before today's date above." + tail
	}
	return "- Your training data ends well before today's date above." + tail
}

// withKnowledgeCutoff inserts the note into the environment block, on the line
// after the date. ts must be the timestamp the prompt was built with, which is
// what makes the anchor exact.
//
// The second return reports whether the anchor was found. Callers in the server
// ignore it and take the appended fallback, but a test asserts it is true, so
// that a change to the upstream environment block fails loudly here instead of
// silently moving the note somewhere it reads as an afterthought.
func withKnowledgeCutoff(prompt string, ts time.Time, cutoff string) (string, bool) {
	note := cutoffNote(cutoff)
	anchor := "- Date: " + ts.Format("2006-01-02")
	i := strings.Index(prompt, anchor)
	if i < 0 {
		return prompt + "\n\n" + note, false
	}
	at := i + len(anchor)
	return prompt[:at] + "\n" + note + prompt[at:], true
}

// swapCutoffNote replaces the note in a prompt already built, for a session
// that switches model. Only this line is touched: rebuilding the whole prompt
// would re-read the workspace's AGENTS.md, and a session's instructions should
// no more change under it than its scene does — the more so because the model
// itself can write files in that workspace.
//
// It reports whether the previous note was found; the caller rebuilds when it
// was not, so a prompt assembled some other way still ends up correct.
func swapCutoffNote(prompt, previous, next string) (string, bool) {
	was := cutoffNote(previous)
	i := strings.Index(prompt, was)
	if i < 0 {
		return prompt, false
	}
	return prompt[:i] + cutoffNote(next) + prompt[i+len(was):], true
}

// subagentEnvironment is the environment block a sub-agent gets. Its own prompt
// is short and shares nothing with the session's, but it runs on a model of its
// own — possibly a different one from the parent's — and has the same web tools,
// so it needs the same date and the same note.
func subagentEnvironment(ts time.Time, cutoff string) string {
	return "\n\nEnvironment:\n- Date: " + ts.Format("2006-01-02") + "\n" + cutoffNote(cutoff)
}

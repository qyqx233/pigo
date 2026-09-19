package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/session"
)

func summaryMessage(text string) agentcore.CompactionMessage {
	return agentcore.CompactionMessage{
		RoleField:    agentcore.RoleCompaction,
		Summary:      text,
		TokensBefore: 1000,
		Details:      json.RawMessage(`{"readFiles":["a.go"]}`),
	}
}

// texts renders messages as short strings for comparison: a summary as
// "C:<summary>", anything else as its text.
func texts(msgs []agentcore.Message) string {
	var out []string
	for _, m := range msgs {
		switch v := m.(type) {
		case agentcore.CompactionMessage:
			out = append(out, "C:"+v.Summary)
		case agentcore.UserMessage:
			out = append(out, agentcore.ContentToText(v.Content))
		case agentcore.AssistantMessage:
			out = append(out, agentcore.ContentToText(v.Content))
		}
	}
	return strings.Join(out, ",")
}

func entryMessages(entries []session.Entry) []agentcore.Message {
	out := make([]agentcore.Message, len(entries))
	for i, e := range entries {
		out[i] = e.Message
	}
	return out
}

func mustSave(t *testing.T, s *apiServer, managed *managedSession, msgs agentcore.MessageList) bool {
	t.Helper()
	rewrote, err := s.saveTranscript(managed, msgs)
	if err != nil {
		t.Fatal(err)
	}
	return rewrote
}

// reload loads the transcript into a fresh session, as a restart does.
func reload(t *testing.T, s *apiServer, managed *managedSession) (*managedSession, agentcore.MessageList) {
	t.Helper()
	fresh := &managedSession{paths: managed.paths, meta: managed.meta}
	return fresh, s.loadTranscript(fresh)
}

func TestTranscriptKeepsHistoryThroughCompaction(t *testing.T) {
	s, managed := transcriptSession(t)
	u := func(x string) agentcore.Message { return textMessage(agentcore.RoleUser, x) }
	a := func(x string) agentcore.Message { return textMessage(agentcore.RoleAssistant, x) }

	// A new session's first save creates the file: nothing moved.
	if mustSave(t, s, managed, agentcore.MessageList{u("u1")}) {
		t.Error("creating the file counted as a rewrite")
	}
	mustSave(t, s, managed, agentcore.MessageList{u("u1"), a("a1"), u("u2"), a("a2")})

	// Compaction: the summary replaces u1,a1 and keeps u2,a2.
	ctx := agentcore.MessageList{summaryMessage("S1"), u("u2"), a("a2")}
	if mustSave(t, s, managed, ctx) {
		t.Fatal("a compaction rewrote the file")
	}
	entries, _ := readTranscript(managed.paths)
	if got := texts(entryMessages(entries)); got != "u1,a1,u2,a2,C:S1" {
		t.Fatalf("file = %s, want the whole history plus the summary", got)
	}
	meta := readCompactionMeta(entries[4].Message.(agentcore.CompactionMessage).Details)
	if meta.FirstKeptEntryID == nil || *meta.FirstKeptEntryID != entries[2].ID || meta.Summarized != 2 || meta.TokensAfter <= 0 {
		t.Errorf("compaction meta = %+v (want first kept %s)", meta, entries[2].ID)
	}
	if !strings.Contains(string(entries[4].Message.(agentcore.CompactionMessage).Details), `"readFiles"`) {
		t.Error("the compaction package's own details were lost")
	}

	// The context goes on growing: appended after the summary.
	ctx = append(ctx, u("u3"), a("a3"))
	if mustSave(t, s, managed, ctx) {
		t.Fatal("an append after compaction rewrote the file")
	}

	// A restart rebuilds the same context from the file.
	fresh, loaded := reload(t, s, managed)
	if got := texts(loaded); got != "C:S1,u2,a2,u3,a3" {
		t.Fatalf("reloaded context = %s", got)
	}

	// A second compaction whose kept range spans the first summary's entry:
	// it keeps u2 onwards (entries before C1 in the file), and C1 itself is
	// not part of the new context.
	ctx2 := append(agentcore.MessageList{summaryMessage("S2")}, loaded[1:]...)
	ctx2 = append(ctx2, u("u4")) // and a new message in the same save
	if mustSave(t, s, fresh, ctx2) {
		t.Fatal("the second compaction rewrote the file")
	}
	_, loaded = reload(t, s, fresh)
	if got := texts(loaded); got != "C:S2,u2,a2,u3,a3,u4" {
		t.Fatalf("after the second compaction = %s", got)
	}
	entries, _ = readTranscript(managed.paths)
	if got := texts(entryMessages(entries)); got != "u1,a1,u2,a2,C:S1,u3,a3,C:S2,u4" {
		t.Fatalf("file = %s", got)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].ParentID != entries[i-1].ID {
			t.Fatalf("entry %d's parent is not the one before it", i)
		}
	}
}

func TestTranscriptCompactionKeepingOnlyTheTail(t *testing.T) {
	s, managed := transcriptSession(t)
	u := func(x string) agentcore.Message { return textMessage(agentcore.RoleUser, x) }
	a := func(x string) agentcore.Message { return textMessage(agentcore.RoleAssistant, x) }
	// Repeated messages: the kept tail must be matched as a whole, not at the
	// first equal message.
	mustSave(t, s, managed, agentcore.MessageList{u("go"), a("ok"), u("go"), a("ok")})
	mustSave(t, s, managed, agentcore.MessageList{summaryMessage("S"), u("go"), a("ok"), u("next")})
	_, loaded := reload(t, s, managed)
	if got := texts(loaded); got != "C:S,go,ok,next" {
		t.Fatalf("context = %s", got)
	}
	entries, _ := readTranscript(managed.paths)
	meta := readCompactionMeta(entries[4].Message.(agentcore.CompactionMessage).Details)
	if *meta.FirstKeptEntryID != entries[2].ID {
		t.Errorf("first kept = %s, want the second go (%s)", *meta.FirstKeptEntryID, entries[2].ID)
	}
}

// TestTranscriptCompactionOfUnsavedMessages: the loop compacts before a
// step's checkpoint, so everything it kept can be the step's own messages,
// not yet written. The summary and those messages are appended; nothing
// already saved is lost or repeated.
func TestTranscriptCompactionOfUnsavedMessages(t *testing.T) {
	s, managed := transcriptSession(t)
	u := func(x string) agentcore.Message { return textMessage(agentcore.RoleUser, x) }
	a := func(x string) agentcore.Message { return textMessage(agentcore.RoleAssistant, x) }
	mustSave(t, s, managed, agentcore.MessageList{u("u1"), a("a1"), u("u2")})
	// The step added a2 and a3; compaction kept only those.
	if mustSave(t, s, managed, agentcore.MessageList{summaryMessage("S"), a("a2"), a("a3")}) {
		t.Fatal("rewrote the file")
	}
	entries, _ := readTranscript(managed.paths)
	if got := texts(entryMessages(entries)); got != "u1,a1,u2,C:S,a2,a3" {
		t.Fatalf("file = %s", got)
	}
	meta := readCompactionMeta(entries[3].Message.(agentcore.CompactionMessage).Details)
	if meta.FirstKeptEntryID == nil || *meta.FirstKeptEntryID != "" || meta.Summarized != 3 {
		t.Errorf("meta = %+v, want nothing saved kept and 3 summarized", meta)
	}
	_, loaded := reload(t, s, managed)
	if got := texts(loaded); got != "C:S,a2,a3" {
		t.Fatalf("context = %s", got)
	}
}

// TestTranscriptLegacyCompaction: a file from before, rewritten by a
// compaction (summary first, no kept marker), loads as it did.
func TestTranscriptLegacyCompaction(t *testing.T) {
	s, managed := transcriptSession(t)
	u := func(x string) agentcore.Message { return textMessage(agentcore.RoleUser, x) }
	legacy := summaryMessage("old")
	legacy.Details = json.RawMessage(`{"readFiles":[]}`)
	entries := []session.Entry{
		{ID: "e1", Message: legacy},
		{ID: "e2", ParentID: "e1", Message: u("kept")},
		{ID: "e3", ParentID: "e2", Message: u("after")},
	}
	if err := rewriteTranscript(managed, entries, managed.meta.CreatedAt); err != nil {
		t.Fatal(err)
	}
	_, loaded := reload(t, s, managed)
	if got := texts(loaded); got != "C:old,kept,after" {
		t.Fatalf("context = %s", got)
	}
}

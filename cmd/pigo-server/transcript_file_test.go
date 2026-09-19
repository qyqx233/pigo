package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
)

func transcriptSession(t *testing.T) (*apiServer, *managedSession) {
	t.Helper()
	paths := newSessionPaths(t.TempDir(), "s1")
	if err := paths.create(); err != nil {
		t.Fatal(err)
	}
	return &apiServer{}, &managedSession{paths: paths, meta: sessionMeta{ID: "s1", CreatedAt: time.Now()}}
}

// TestTranscriptAppends: a step appends to the file without touching what is
// already there; ids and times stay; a restart picks up where it left off.
func TestTranscriptAppends(t *testing.T) {
	s, managed := transcriptSession(t)
	path := transcriptPath(managed.paths)
	msgs := agentcore.MessageList{textMessage(agentcore.RoleUser, "hi"), textMessage(agentcore.RoleAssistant, "hello")}
	if err := s.saveTranscript(managed, msgs); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	first, _ := readTranscript(managed.paths)

	time.Sleep(2 * time.Millisecond)
	msgs = append(msgs, textMessage(agentcore.RoleUser, "more"), textMessage(agentcore.RoleAssistant, "sure"))
	if err := s.saveTranscript(managed, msgs); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.HasPrefix(after, before) {
		t.Fatal("the existing content was rewritten, not appended to")
	}
	entries, err := readTranscript(managed.paths)
	if err != nil || len(entries) != 4 {
		t.Fatalf("entries = %d %v", len(entries), err)
	}
	for i := range first {
		if entries[i].ID != first[i].ID || !entries[i].Timestamp.Equal(first[i].Timestamp) {
			t.Errorf("entry %d changed: %+v -> %+v", i, first[i], entries[i])
		}
	}
	if entries[2].ParentID != entries[1].ID || !entries[2].Timestamp.After(first[1].Timestamp) {
		t.Errorf("new entry not chained: %+v", entries[2])
	}
	// Nothing new: nothing written.
	if err := s.saveTranscript(managed, msgs); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(path); !bytes.Equal(again, after) {
		t.Error("an unchanged transcript was written")
	}

	// A restart: the session loads the file and appends to it.
	fresh := &managedSession{paths: managed.paths, meta: managed.meta}
	loaded := s.loadTranscript(fresh)
	if len(loaded) != 4 || !fresh.transcript.synced {
		t.Fatalf("loaded %d synced=%v", len(loaded), fresh.transcript.synced)
	}
	if err := s.saveTranscript(fresh, append(loaded, textMessage(agentcore.RoleUser, "again"))); err != nil {
		t.Fatal(err)
	}
	final, _ := os.ReadFile(path)
	if !bytes.HasPrefix(final, after) {
		t.Fatal("after a restart the file was rewritten, not appended to")
	}
	if entries, _ := readTranscript(managed.paths); len(entries) != 5 || entries[4].ParentID != entries[3].ID {
		t.Fatalf("after restart = %+v", entries)
	}
}

// TestTranscriptRewrites: a history that changed under the stored part — a
// compaction — rewrites the file whole.
func TestTranscriptRewrites(t *testing.T) {
	s, managed := transcriptSession(t)
	msgs := agentcore.MessageList{textMessage(agentcore.RoleUser, "a"), textMessage(agentcore.RoleAssistant, "b"), textMessage(agentcore.RoleUser, "c")}
	if err := s.saveTranscript(managed, msgs); err != nil {
		t.Fatal(err)
	}
	// Compaction: shorter, with a summary first.
	compacted := agentcore.MessageList{textMessage(agentcore.RoleUser, "summary"), textMessage(agentcore.RoleUser, "c")}
	if err := s.saveTranscript(managed, compacted); err != nil {
		t.Fatal(err)
	}
	entries, _ := readTranscript(managed.paths)
	if len(entries) != 2 || agentcore.ContentToText(entries[0].Message.(agentcore.UserMessage).Content) != "summary" {
		t.Fatalf("after compaction = %+v", entries)
	}
	// Longer, but the first message is new: also a rewrite, not an append.
	edited := agentcore.MessageList{textMessage(agentcore.RoleUser, "summary v2"), textMessage(agentcore.RoleUser, "c"), textMessage(agentcore.RoleAssistant, "d")}
	if err := s.saveTranscript(managed, edited); err != nil {
		t.Fatal(err)
	}
	entries, _ = readTranscript(managed.paths)
	if len(entries) != 3 || agentcore.ContentToText(entries[0].Message.(agentcore.UserMessage).Content) != "summary v2" {
		t.Fatalf("after edit = %+v", entries)
	}
}

// TestTranscriptTornTail: a crash in the middle of an append leaves the last
// line incomplete; loading drops that line and keeps the rest.
func TestTranscriptTornTail(t *testing.T) {
	for name, tail := range map[string]string{
		"partial line":   `{"id":"x","parentId":"y","timestamp":"2026-09-19T00:00:00Z","message":{"role":"us`,
		"unparsed line":  "{garbage}\n",
		"trailing blank": "",
	} {
		t.Run(name, func(t *testing.T) {
			s, managed := transcriptSession(t)
			msgs := agentcore.MessageList{textMessage(agentcore.RoleUser, "hi"), textMessage(agentcore.RoleAssistant, "hello")}
			if err := s.saveTranscript(managed, msgs); err != nil {
				t.Fatal(err)
			}
			f, _ := os.OpenFile(transcriptPath(managed.paths), os.O_APPEND|os.O_WRONLY, 0o600)
			_, _ = f.WriteString(tail)
			_ = f.Close()

			fresh := &managedSession{paths: managed.paths, meta: managed.meta}
			loaded := s.loadTranscript(fresh)
			if len(loaded) != 2 {
				t.Fatalf("loaded %d messages, want the 2 intact ones", len(loaded))
			}
			// And the session carries on appending to the repaired file.
			if err := s.saveTranscript(fresh, append(loaded, textMessage(agentcore.RoleUser, "next"))); err != nil {
				t.Fatal(err)
			}
			if entries, err := readTranscript(managed.paths); err != nil || len(entries) != 3 {
				t.Fatalf("after repair = %d %v", len(entries), err)
			}
		})
	}
}

// TestTranscriptUnreadableSetAside: a file damaged beyond its last line is
// moved aside, never overwritten, and the session starts afresh.
func TestTranscriptUnreadableSetAside(t *testing.T) {
	s, managed := transcriptSession(t)
	path := transcriptPath(managed.paths)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a header\n{\"id\":\"a\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if loaded := s.loadTranscript(managed); len(loaded) != 0 {
		t.Fatalf("loaded %d", len(loaded))
	}
	matches, _ := filepath.Glob(path + ".unreadable-*")
	if len(matches) != 1 {
		t.Fatalf("the damaged file was not set aside: %v", matches)
	}
	if data, _ := os.ReadFile(matches[0]); !strings.Contains(string(data), "not a header") {
		t.Error("the set-aside file lost its content")
	}
	if err := s.saveTranscript(managed, agentcore.MessageList{textMessage(agentcore.RoleUser, "hi")}); err != nil {
		t.Fatal(err)
	}
	if entries, err := readTranscript(managed.paths); err != nil || len(entries) != 1 {
		t.Fatalf("fresh transcript = %d %v", len(entries), err)
	}
}

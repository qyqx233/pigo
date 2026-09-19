// A session's transcript: sessions/<id>/transcript/chat.jsonl, in the upstream
// session-file format (a header line, then one entry per message: id, parent
// id, time, message).
//
// The file is appended to as the turn goes — turnRun's checkpoint after every
// step writes only the step's new messages, in one write at the end of the
// file — so a long session costs the same per step as a short one, and a
// message keeps its id and time once written. The rare step that rewrites
// earlier messages (context compaction) rewrites the file whole, through a
// temporary file and a rename.
//
// An append interrupted by a crash can leave only the last line incomplete.
// Loading drops such a line, losing at most the step in progress.
//
// The header's UpdatedAt is set when the file is created or rewritten and not
// on appends (updating it would mean rewriting the file). The server does not
// read it; the sessions table is authoritative.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/session"
)

// transcriptID is the session-store id of every transcript file: chat.jsonl.
const transcriptID = "chat"

// transcriptState is what a session knows about its transcript file, so a
// checkpoint can tell "messages were added" from "messages were rewritten".
// Guarded by managedSession.mu.
//
// Rewrites are recognised by the stored first and last messages rather than by
// comparing every message, which would re-encode the whole history each step.
// That covers every way the agent rewrites history: compaction puts a new
// summary first (and usually shortens the list), and a retry only drops
// messages of the step in progress, which were never stored.
type transcriptState struct {
	// synced is set once the state below matches the file: after it was
	// loaded or written.
	synced bool
	// count is how many messages the file holds.
	count int
	// first and last are the stored first and last messages, encoded; lastID
	// is the last one's entry id.
	first, last string
	lastID      string
}

func transcriptDir(paths sessionPaths) string { return filepath.Join(paths.Root, "transcript") }

func transcriptPath(paths sessionPaths) string {
	return filepath.Join(transcriptDir(paths), session.FileName(transcriptID))
}

func encodeMessage(m agentcore.Message) (string, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// repairTranscriptTail drops an incomplete or unreadable last line — what a
// crash in the middle of an append leaves. Appends only ever touch the end of
// the file, so nothing earlier is checked.
func repairTranscriptTail(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	keep := len(data)
	if keep > 0 && data[keep-1] != '\n' {
		// A partial last line.
		keep = bytes.LastIndexByte(data, '\n') + 1
	} else if keep > 0 {
		// A complete last line that does not parse (a torn write can end on
		// a newline inside a string only if the payload had one; entries are
		// single-line JSON, so this is belt and braces).
		body := data[:keep-1]
		start := bytes.LastIndexByte(body, '\n') + 1
		if start > 0 && !json.Valid(body[start:]) {
			keep = start
		}
	}
	if keep == len(data) {
		return nil
	}
	if keep == 0 {
		// Not even a header: nothing to keep.
		return os.Remove(path)
	}
	log.Printf("pigo-server: %s: dropping an incomplete last line (%d bytes) left by an interrupted write", path, len(data)-keep)
	return os.Truncate(path, int64(keep))
}

// readTranscript reads a session's transcript entries, repairing a torn tail
// first. A missing file is an empty transcript. Caller holds managed.mu.
func readTranscript(paths sessionPaths) ([]session.Entry, error) {
	path := transcriptPath(paths)
	if err := repairTranscriptTail(path); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	store, err := session.NewStore(transcriptDir(paths))
	if err != nil {
		return nil, err
	}
	_, entries, err := store.LoadEntries(transcriptID)
	return entries, err
}

// loadTranscript reads a session's messages for the agent and records what the
// file holds. A file that cannot be read is set aside (renamed, never
// overwritten) and the session starts from an empty transcript. Caller holds
// managed.mu.
func (s *apiServer) loadTranscript(managed *managedSession) agentcore.MessageList {
	entries, err := readTranscript(managed.paths)
	if err != nil {
		path := transcriptPath(managed.paths)
		aside := fmt.Sprintf("%s.unreadable-%s", path, time.Now().Format("20060102-150405"))
		log.Printf("pigo-server: session %s: transcript unreadable (%v); moved to %s", shortID(managed.meta.ID), err, aside)
		_ = os.Rename(path, aside)
		managed.transcript = transcriptState{}
		return nil
	}
	msgs := make(agentcore.MessageList, 0, len(entries))
	for _, e := range entries {
		msgs = append(msgs, e.Message)
	}
	state := transcriptState{synced: true, count: len(msgs)}
	if n := len(entries); n > 0 {
		// Re-encoded rather than taken from the file, so the comparison in
		// saveTranscript compares like with like.
		state.first, _ = encodeMessage(msgs[0])
		state.last, _ = encodeMessage(msgs[n-1])
		state.lastID = entries[n-1].ID
	}
	managed.transcript = state
	return msgs
}

// loadTranscriptEntries reads a session's transcript for history. Caller holds
// managed.mu.
func (s *apiServer) loadTranscriptEntries(managed *managedSession) []session.Entry {
	entries, err := readTranscript(managed.paths)
	if err != nil {
		log.Printf("pigo-server: session %s: read transcript: %v", shortID(managed.meta.ID), err)
		return nil
	}
	return entries
}

// saveTranscript stores msgs as the session's transcript. When the file's
// messages are a prefix of msgs — the normal case, a step added messages —
// only the new ones are appended. Otherwise (the history was rewritten, or the
// file's state is unknown) the file is rewritten whole. Caller holds
// managed.mu.
func (s *apiServer) saveTranscript(managed *managedSession, msgs agentcore.MessageList) error {
	state := managed.transcript
	appendOnly := state.synced && state.count > 0 && state.count <= len(msgs)
	if appendOnly {
		first, err1 := encodeMessage(msgs[0])
		last, err2 := encodeMessage(msgs[state.count-1])
		appendOnly = err1 == nil && err2 == nil && first == state.first && last == state.last
	}
	if appendOnly && state.count == len(msgs) {
		return nil
	}
	if !appendOnly && state.synced && state.count == 0 && len(msgs) == 0 {
		return nil
	}

	from, parent := 0, ""
	if appendOnly {
		from, parent = state.count, state.lastID
	}
	now := time.Now().UTC()
	next := transcriptState{synced: true, count: len(msgs), first: state.first}
	entries := make([]session.Entry, 0, len(msgs)-from)
	for i := from; i < len(msgs); i++ {
		body, err := encodeMessage(msgs[i])
		if err != nil {
			managed.transcript.synced = false
			return fmt.Errorf("encode message %d: %w", i, err)
		}
		e := session.Entry{ID: newMessageEntryID(), ParentID: parent, Timestamp: now, Message: msgs[i]}
		entries = append(entries, e)
		parent = e.ID
		if i == 0 {
			next.first = body
		}
		next.last, next.lastID = body, e.ID
	}

	var err error
	if appendOnly {
		err = appendTranscriptEntries(transcriptPath(managed.paths), entries)
	} else {
		err = rewriteTranscript(managed, entries, now)
	}
	if err != nil {
		// What reached the file is uncertain: the next save rewrites it.
		managed.transcript.synced = false
		return err
	}
	managed.transcript = next
	return nil
}

// appendTranscriptEntries writes entries at the end of the file in one write.
func appendTranscriptEntries(path string, entries []session.Entry) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// rewriteTranscript replaces the file with header and entries, atomically.
func rewriteTranscript(managed *managedSession, entries []session.Entry, now time.Time) error {
	store, err := session.NewStore(transcriptDir(managed.paths))
	if err != nil {
		return err
	}
	return store.SaveEntries(session.SessionHeader{
		ID:        transcriptID,
		CreatedAt: managed.meta.CreatedAt,
		UpdatedAt: now,
		Model:     managed.meta.Model,
		Provider:  managed.meta.Provider,
		Cwd:       virtualWorkspaceRoot,
	}, entries)
}

// hasTranscript reports whether a session has a transcript file.
func hasTranscript(paths sessionPaths) bool {
	entries, err := os.ReadDir(transcriptDir(paths))
	return err != nil && !errors.Is(err, os.ErrNotExist) || len(entries) > 0
}

// newMessageEntryID matches the upstream session store's entry ids: 8 hex
// characters, unique within a transcript.
func newMessageEntryID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

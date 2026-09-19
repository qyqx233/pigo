// A session's transcript: sessions/<id>/transcript/chat.jsonl, in the upstream
// session-file format (a header line, then one entry per message: id, parent
// id, time, message).
//
// The file is appended to as the turn goes — turnRun's checkpoint after every
// step writes only the step's new messages, in one write at the end of the
// file — so a long session costs the same per step as a short one, and a
// message keeps its id and time once written.
//
// The file is the session's whole history, and the agent's context is a view
// of it. Context compaction does not rewrite the file: it appends the summary
// as a compaction entry that names the first entry kept verbatim, and loading
// rebuilds the context from the last such entry — the summary, the kept
// entries, and everything after it — while the history the web shows reads the
// whole file. Anything else that rewrites earlier messages (none of the web's
// paths do) rewrites the file whole, through a temporary file and a rename.
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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/compaction"
	"github.com/smallnest/pigo/internal/session"
)

// transcriptID is the session-store id of every transcript file: chat.jsonl.
const transcriptID = "chat"

// transcriptState is what a session knows about its transcript file, so a
// checkpoint can tell "messages were added" from "the context was compacted"
// from "messages were rewritten". Guarded by managedSession.mu.
//
// view lists the entries behind the agent's context, in context order, with a
// hash of each message. Appends are recognised by the first and last of them
// alone, so a step does not re-encode the whole history; a compaction is
// recognised by a new compaction message first, followed by a tail of the old
// view.
type transcriptState struct {
	// synced is set once the state below matches the file: after it was
	// loaded or written.
	synced bool
	view   []viewEntry
	// fileCount is how many entries the file holds; lastID is the last one's.
	fileCount int
	lastID    string
}

// viewEntry is one message of the agent's context: its entry in the file, and
// a hash of the message as the agent holds it.
type viewEntry struct {
	id   string
	hash [32]byte
}

func messageHash(m agentcore.Message) ([32]byte, error) {
	body, err := encodeMessage(m)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256([]byte(body)), nil
}

// compactionMeta is what the server adds to a compaction entry's details, next
// to the compaction package's own fields.
type compactionMeta struct {
	// FirstKeptEntryID is the first entry the compaction kept verbatim; ""
	// when it kept none. Absent in entries written before the file kept its
	// whole history, which then stood first in a rewritten file.
	FirstKeptEntryID *string `json:"firstKeptEntryId,omitempty"`
	// Summarized is how many context messages the summary replaced;
	// TokensAfter the context's estimated size after it.
	Summarized  int `json:"summarized,omitempty"`
	TokensAfter int `json:"tokensAfter,omitempty"`
}

func readCompactionMeta(details json.RawMessage) compactionMeta {
	var meta compactionMeta
	_ = json.Unmarshal(details, &meta)
	return meta
}

// withCompactionMeta merges meta into a compaction message's details, keeping
// the fields already there.
func withCompactionMeta(m agentcore.CompactionMessage, meta compactionMeta) agentcore.CompactionMessage {
	fields := map[string]json.RawMessage{}
	_ = json.Unmarshal(m.Details, &fields)
	add, _ := json.Marshal(meta)
	var extra map[string]json.RawMessage
	_ = json.Unmarshal(add, &extra)
	for k, v := range extra {
		fields[k] = v
	}
	if merged, err := json.Marshal(fields); err == nil {
		m.Details = merged
	}
	return m
}

// contextView rebuilds the agent's context from the file's entries: from the
// last compaction entry on, its summary, the entries it kept (skipping earlier
// compaction entries among them, which that summary replaced), and everything
// after it.
func contextView(entries []session.Entry) []session.Entry {
	last := -1
	for i := len(entries) - 1; i >= 0; i-- {
		if _, ok := entries[i].Message.(agentcore.CompactionMessage); ok {
			last = i
			break
		}
	}
	if last < 0 {
		return entries
	}
	view := []session.Entry{entries[last]}
	meta := readCompactionMeta(entries[last].Message.(agentcore.CompactionMessage).Details)
	if meta.FirstKeptEntryID != nil && *meta.FirstKeptEntryID != "" {
		for i := 0; i < last; i++ {
			if entries[i].ID != *meta.FirstKeptEntryID {
				continue
			}
			for _, e := range entries[i:last] {
				if _, isCompaction := e.Message.(agentcore.CompactionMessage); !isCompaction {
					view = append(view, e)
				}
			}
			break
		}
	}
	return append(view, entries[last+1:]...)
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
	view := contextView(entries)
	msgs := make(agentcore.MessageList, 0, len(view))
	state := transcriptState{synced: true, fileCount: len(entries), view: make([]viewEntry, 0, len(view))}
	for _, e := range view {
		hash, err := messageHash(e.Message)
		if err != nil {
			state.synced = false // the next save rewrites the file
		}
		msgs = append(msgs, e.Message)
		state.view = append(state.view, viewEntry{id: e.ID, hash: hash})
	}
	if n := len(entries); n > 0 {
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

// saveTranscript stores msgs — the agent's context — in the session's
// transcript, and reports whether it had to rewrite the file. Caller holds
// managed.mu.
//
//   - The usual case: the context grew. The new messages are appended.
//   - The context was compacted: it now starts with a new compaction message,
//     followed by a tail of what it held before. The compaction message is
//     appended, naming the first kept entry, then any messages after the kept
//     tail; nothing already written changes.
//   - Anything else — the file's state unknown, or history rewritten some
//     other way — rewrites the file with msgs.
func (s *apiServer) saveTranscript(managed *managedSession, msgs agentcore.MessageList) (rewrote bool, err error) {
	state := managed.transcript
	hashes := make([][32]byte, len(msgs))
	hashOf := func(i int) ([32]byte, error) {
		if hashes[i] == ([32]byte{}) {
			h, err := messageHash(msgs[i])
			if err != nil {
				return h, fmt.Errorf("encode message %d: %w", i, err)
			}
			hashes[i] = h
		}
		return hashes[i], nil
	}
	fail := func(err error) (bool, error) {
		managed.transcript.synced = false
		return false, err
	}

	n := len(state.view)
	if state.synced && state.fileCount == 0 && len(msgs) == 0 {
		return false, nil
	}
	if state.synced && state.fileCount > 0 && n > 0 && n <= len(msgs) {
		first, err := hashOf(0)
		if err != nil {
			return fail(err)
		}
		last, err := hashOf(n - 1)
		if err != nil {
			return fail(err)
		}
		if first == state.view[0].hash && last == state.view[n-1].hash {
			if n == len(msgs) {
				return false, nil
			}
			return false, s.appendToTranscript(managed, msgs, hashOf, n, state.view, nil)
		}
	}
	if len(msgs) > 0 && state.synced && state.fileCount > 0 {
		if found, err := s.compactionAppend(managed, msgs, hashOf); found {
			return false, err
		}
	}
	return s.rewriteWith(managed, msgs, hashOf, fail)
}

// compactionAppend handles a context that now starts with a new compaction
// message. It reports found=false when msgs is not that, or when the kept
// messages cannot be placed in the old view — the caller then rewrites.
func (s *apiServer) compactionAppend(managed *managedSession, msgs agentcore.MessageList, hashOf func(int) ([32]byte, error)) (found bool, err error) {
	state := managed.transcript
	n := len(state.view)
	summaryMsg, ok := msgs[0].(agentcore.CompactionMessage)
	if !ok {
		return false, nil
	}
	if n > 0 {
		if h, err := hashOf(0); err == nil && h == state.view[0].hash {
			return false, nil // the same compaction as before: not new
		}
	}
	k, placed := keptTail(state.view, len(msgs)-1, hashOf)
	if !placed {
		log.Printf("pigo-server: session %s: a compacted message cannot be encoded; rewriting the transcript", shortID(managed.meta.ID))
		return false, nil
	}
	meta := compactionMeta{Summarized: k, TokensAfter: compaction.EstimateContextTokens(msgs).Tokens}
	firstKept := ""
	if k < n {
		firstKept = state.view[k].id
	}
	meta.FirstKeptEntryID = &firstKept
	summary := withCompactionMeta(summaryMsg, meta)
	// The view keeps the hash of the message as the agent holds it; the
	// file's copy carries the added details.
	return true, s.appendToTranscript(managed, msgs, hashOf, 0, state.view[k:], &summary)
}

// rewriteWith replaces the file with msgs. It reports rewrote=true unless the
// file was known to be empty: only then did no entry move.
func (s *apiServer) rewriteWith(managed *managedSession, msgs agentcore.MessageList, hashOf func(int) ([32]byte, error), fail func(error) (bool, error)) (bool, error) {
	wasEmpty := managed.transcript.synced && managed.transcript.fileCount == 0
	if !managed.transcript.synced {
		_, statErr := os.Stat(transcriptPath(managed.paths))
		wasEmpty = errors.Is(statErr, os.ErrNotExist)
	}
	now := time.Now().UTC()
	next := transcriptState{synced: true, fileCount: len(msgs), view: make([]viewEntry, 0, len(msgs))}
	entries := make([]session.Entry, 0, len(msgs))
	parent := ""
	for i := range msgs {
		hash, err := hashOf(i)
		if err != nil {
			return fail(err)
		}
		e := session.Entry{ID: newMessageEntryID(), ParentID: parent, Timestamp: now, Message: msgs[i]}
		entries = append(entries, e)
		parent = e.ID
		next.view = append(next.view, viewEntry{id: e.ID, hash: hash})
		next.lastID = e.ID
	}
	if err := rewriteTranscript(managed, entries, now); err != nil {
		// What reached the file is uncertain: the next save rewrites it.
		return fail(err)
	}
	managed.transcript = next
	return !wasEmpty, nil
}

// transcriptLen is how many entries the session's transcript file holds, or
// -1 when that is not known.
func (s *apiServer) transcriptLen(managed *managedSession) int {
	managed.mu.Lock()
	defer managed.mu.Unlock()
	if !managed.transcript.synced {
		return -1
	}
	return managed.transcript.fileCount
}

// keptTail finds where a compaction's kept messages start in the old view: the
// first k whose view[k:] equals the new context's messages 1.. one for one.
// When none does, the compaction kept nothing already saved: the loop compacts
// before a step's checkpoint, so the kept messages can all be that step's, not
// yet written — k is then len(view), and every message after the summary is
// new. It reports false only when a message cannot be hashed.
func keptTail(view []viewEntry, afterSummary int, hashOf func(int) ([32]byte, error)) (int, bool) {
	n := len(view)
	for i := 1; i <= afterSummary; i++ {
		if _, err := hashOf(i); err != nil {
			return 0, false
		}
	}
	for k := 0; k < n; k++ {
		if n-k > afterSummary {
			continue
		}
		match := true
		for j := 0; j < n-k; j++ {
			h, err := hashOf(1 + j)
			if err != nil || h != view[k+j].hash {
				match = false
				break
			}
		}
		if match {
			return k, true
		}
	}
	return n, true
}

// appendToTranscript appends msgs[from:] to the file. With summary set (a
// compaction), msgs[0] is written as summary instead, and the kept view
// follows it in the new view; msgs[1+len(kept):] are the messages after.
func (s *apiServer) appendToTranscript(managed *managedSession, msgs agentcore.MessageList, hashOf func(int) ([32]byte, error), from int, kept []viewEntry, summary *agentcore.CompactionMessage) error {
	state := managed.transcript
	now := time.Now().UTC()
	parent := state.lastID
	var entries []session.Entry
	var view []viewEntry
	add := func(i int, m agentcore.Message) error {
		hash, err := hashOf(i)
		if err != nil {
			return err
		}
		e := session.Entry{ID: newMessageEntryID(), ParentID: parent, Timestamp: now, Message: m}
		entries = append(entries, e)
		parent = e.ID
		view = append(view, viewEntry{id: e.ID, hash: hash})
		return nil
	}
	if summary != nil {
		if err := add(0, *summary); err != nil {
			managed.transcript.synced = false
			return err
		}
		view = append(view, kept...)
		from = 1 + len(kept)
	} else {
		view = append(view, state.view...)
	}
	for i := from; i < len(msgs); i++ {
		if err := add(i, msgs[i]); err != nil {
			managed.transcript.synced = false
			return err
		}
	}
	if err := appendTranscriptEntries(transcriptPath(managed.paths), entries); err != nil {
		// What reached the file is uncertain: the next save rewrites it.
		managed.transcript.synced = false
		return err
	}
	managed.transcript = transcriptState{synced: true, view: view, fileCount: state.fileCount + len(entries), lastID: parent}
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

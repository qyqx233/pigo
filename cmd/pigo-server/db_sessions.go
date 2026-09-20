// Sessions in the database: a session's metadata (owner, model, title, the
// running and last turn …) is one row of the sessions table, a JSON document.
// Its transcript is a file (transcript_file.go).
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
)

// --- sessions -------------------------------------------------------------------------

// loadSessions reads every session.
func loadSessions(db *sqlDB, dataDir string) (map[string]*managedSession, error) {
	out := make(map[string]*managedSession)
	var metas []sessionMeta
	if err := db.query("SELECT meta FROM sessions", func(rows *sql.Rows) error {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return err
		}
		var meta sessionMeta
		if err := json.Unmarshal([]byte(doc), &meta); err != nil || meta.ID == "" {
			log.Printf("pigo-server: skipping unreadable session metadata: %v", err)
			return nil
		}
		metas = append(metas, meta)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("load sessions: %w", err)
	}
	for _, meta := range metas {
		paths := newSessionPaths(dataDir, meta.ID)
		_ = os.MkdirAll(paths.Run, 0o700)
		if recoverInterruptedTurn(&meta) {
			if err := upsertSession(db, meta); err != nil {
				return nil, err
			}
		}
		out[meta.ID] = &managedSession{paths: paths, meta: meta}
	}
	return out, nil
}

func upsertSession(e sqlExec, meta sessionMeta) error {
	if meta.draft {
		return nil // stored when its first message arrives (materialize)
	}
	doc, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	_, err = e.exec(`INSERT INTO sessions (id, user_id, created_at, meta) VALUES (?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET user_id = excluded.user_id, meta = excluded.meta`,
		meta.ID, meta.UserID, toNanos(meta.CreatedAt), string(doc))
	return err
}

// saveSession stores a session's metadata. Callers hold the session's lock.
func (s *apiServer) saveSession(meta sessionMeta) error {
	if err := upsertSession(s.db, meta); err != nil {
		log.Printf("pigo-server: session %s: save metadata: %v", shortID(meta.ID), err)
		return err
	}
	return nil
}

// removeSession deletes a session: its row, then its directory (workspace and
// transcript). The ledger
// keeps the session's calls. Callers have stopped its turn and sandbox.
func (s *apiServer) removeSession(id string, paths sessionPaths) error {
	if _, err := s.db.exec("DELETE FROM sessions WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete session record: %w", err)
	}
	return os.RemoveAll(paths.Root)
}

// maxDraftsPerUser bounds the drafts a user can hold in memory: each 新会话
// click makes one, and a draft only ends by being used or dropped here.
const maxDraftsPerUser = 3

// dropOldDrafts forgets a user's oldest drafts, keeping room for one more.
// Caller holds s.mu.
func (s *apiServer) dropOldDrafts(userID string) {
	var drafts []*managedSession
	for _, managed := range s.sessions {
		managed.mu.Lock()
		if managed.meta.draft && managed.meta.UserID == userID && managed.activeTurn() == nil {
			drafts = append(drafts, managed)
		}
		managed.mu.Unlock()
	}
	if len(drafts) < maxDraftsPerUser {
		return
	}
	sort.Slice(drafts, func(i, j int) bool { return drafts[i].meta.CreatedAt.Before(drafts[j].meta.CreatedAt) })
	for _, managed := range drafts[:len(drafts)-maxDraftsPerUser+1] {
		delete(s.sessions, managed.meta.ID)
	}
}

// storedSessionsLocked counts the sessions that are not drafts, which is what
// the session limit is about. Caller holds s.mu.
func (s *apiServer) storedSessionsLocked() int {
	n := 0
	for _, managed := range s.sessions {
		managed.mu.Lock()
		if !managed.meta.draft {
			n++
		}
		managed.mu.Unlock()
	}
	return n
}

// materializeLocked writes a draft session out — its directory and its row —
// when its first message arrives. Caller holds managed.mu.
func (s *apiServer) materializeLocked(managed *managedSession) error {
	if !managed.meta.draft {
		return nil
	}
	if err := managed.paths.create(); err != nil {
		return err
	}
	managed.meta.draft = false
	if err := s.saveSession(managed.meta); err != nil {
		managed.meta.draft = true
		_ = os.RemoveAll(managed.paths.Root)
		return fmt.Errorf("save session: %w", err)
	}
	return nil
}

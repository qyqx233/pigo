// The usage ledger: one line per model call, append-only.
//
// It is an audit record, so it is never rewritten: a price change applies to
// later calls only (each entry carries the price it was charged at), and
// deleting a user or a session leaves their entries in place. It is kept apart
// from session transcripts for the same reason — those are deleted with the
// session, and the money spent is not.
//
// Files are split by month (<dataDir>/ledger/2026-09.jsonl) so a report over a
// period reads only the months it covers.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Entry statuses.
const (
	// usageOK: the call finished and the provider reported its usage.
	usageOK = "ok"
	// usageError: the call failed, but the provider reported usage for it (it
	// charged for what it processed).
	usageError = "error"
	// usageAborted: the call was cut off — the page was reloaded, the
	// connection dropped — after output had started, so tokens were spent that
	// the provider never got to report. Recorded with no token counts rather
	// than a guess.
	usageAborted = "aborted"
	// usageUnreported: the call finished but the provider sent no usage at
	// all. Recorded with no token counts so the gap is visible.
	usageUnreported = "unreported"
)

// Who pays for a call.
const (
	billedPlatform = "platform" // the deployment's shared or environment key
	billedSelf     = "self"     // the user's own key
)

// ledgerEntry is one model call.
type ledgerEntry struct {
	ID         string    `json:"id"`
	At         time.Time `json:"at"`
	UserID     string    `json:"userId"`
	Username   string    `json:"username"`
	SessionID  string    `json:"sessionId"`
	TurnID     string    `json:"turnId"`
	ResponseID string    `json:"responseId,omitempty"`

	Provider      string `json:"provider"`
	Model         string `json:"model"`
	ResponseModel string `json:"responseModel,omitempty"`
	// Kind is "chat" or "compaction" (the summary call that shrinks a long
	// context).
	Kind   string `json:"kind"`
	Status string `json:"status"`

	KeySource string `json:"keySource"`
	BilledTo  string `json:"billedTo"`

	Input      int `json:"input"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
	Output     int `json:"output"`
	Reasoning  int `json:"reasoning,omitempty"`

	// Price is what the call was charged at; nil when the model is unpriced.
	Price  *priceSnapshot `json:"price,omitempty"`
	Priced bool           `json:"priced"`
	// Cost is in nano-yuan (1e-9 yuan).
	Cost int64 `json:"cost"`

	UpstreamCostUSD *float64 `json:"upstreamCostUsd,omitempty"`
	UsageAnomaly    bool     `json:"usageAnomaly,omitempty"`
}

// billedToFor maps where a call's key came from to who pays for it.
func billedToFor(keySource string) string {
	if keySource == "user" {
		return billedSelf
	}
	return billedPlatform
}

type ledgerStore struct {
	mu  sync.Mutex
	dir string
}

func newLedgerStore(dataDir string) (*ledgerStore, error) {
	dir := filepath.Join(dataDir, "ledger")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create ledger dir: %w", err)
	}
	return &ledgerStore{dir: dir}, nil
}

func (l *ledgerStore) monthPath(at time.Time) string {
	return filepath.Join(l.dir, at.UTC().Format("2006-01")+".jsonl")
}

// append writes one entry. Each entry is a single write of one line to a file
// opened in append mode, under a lock, so concurrent calls cannot interleave.
func (l *ledgerStore) append(entry ledgerEntry) error {
	if l == nil {
		return nil
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.monthPath(entry.At), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open ledger: %w", err)
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return fmt.Errorf("write ledger: %w", err)
	}
	return f.Close()
}

// ledgerQuery narrows a scan. Zero fields do not filter.
type ledgerQuery struct {
	From, To  time.Time // [From, To)
	UserID    string
	SessionID string
	Provider  string
	Model     string
}

func (q ledgerQuery) matches(e ledgerEntry) bool {
	if !q.From.IsZero() && e.At.Before(q.From) {
		return false
	}
	if !q.To.IsZero() && !e.At.Before(q.To) {
		return false
	}
	if q.UserID != "" && e.UserID != q.UserID {
		return false
	}
	if q.SessionID != "" && e.SessionID != q.SessionID {
		return false
	}
	if q.Provider != "" && e.Provider != q.Provider {
		return false
	}
	if q.Model != "" && e.Model != q.Model && e.ResponseModel != q.Model {
		return false
	}
	return true
}

// scan returns the entries matching q, oldest first. Only the month files the
// period can touch are read. A malformed line is skipped rather than failing
// the whole report: one bad write must not hide a month of accounting.
func (l *ledgerStore) scan(q ledgerQuery) ([]ledgerEntry, error) {
	if l == nil {
		return nil, nil
	}
	names, err := filepath.Glob(filepath.Join(l.dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var out []ledgerEntry
	for _, name := range names {
		month, err := time.Parse("2006-01", filepath.Base(name[:len(name)-len(".jsonl")]))
		if err != nil {
			continue
		}
		if !q.From.IsZero() && !month.AddDate(0, 1, 0).After(q.From) {
			continue
		}
		if !q.To.IsZero() && !month.Before(q.To) {
			continue
		}
		entries, err := l.readFile(name)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if q.matches(e) {
				out = append(out, e)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

func (l *ledgerStore) readFile(name string) ([]ledgerEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []ledgerEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var e ledgerEntry
		if json.Unmarshal(scanner.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, scanner.Err()
}

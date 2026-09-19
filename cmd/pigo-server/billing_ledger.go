// The usage ledger: one row per model call, append-only.
//
// It is an audit record, so it is never rewritten: a price change applies to
// later calls only (each entry carries the price it was charged at), and
// deleting a user or a session leaves their entries in place. It is kept apart
// from session transcripts for the same reason — those are deleted with the
// session, and the money spent is not.
//
// Reports select by period, user, session and model; the table is indexed for
// each, so a report reads only the rows it covers.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
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

// billedToFor maps where a call's key came from to who pays for it: a user's
// own key bills them, anything else — the shared pool, or the environment in
// entries from before keys stopped coming from there — the platform.
func billedToFor(keySource string) string {
	if keySource == "user" {
		return billedSelf
	}
	return billedPlatform
}

type ledgerStore struct {
	db *sqlDB
}

func newLedgerStore(db *sqlDB) (*ledgerStore, error) {
	return &ledgerStore{db: db}, nil
}

// ledgerColumns lists the columns in the order insertLedgerEntry writes them
// and scanLedgerEntry reads them.
const ledgerColumns = `id, at, user_id, username, session_id, turn_id, response_id, provider, model, response_model,
kind, status, key_source, billed_to, input, cache_read, cache_write, output, reasoning,
price, priced, cost, upstream_cost_usd, usage_anomaly`

// append records one entry at once. The calls of a turn are not written this
// way: they are kept with the turn and committed when it ends (turnRun,
// finishTurn); this is for a call made outside any turn.
func (l *ledgerStore) append(entry ledgerEntry) error {
	if l == nil {
		return nil
	}
	return insertLedgerEntry(l.db, entry)
}

func insertLedgerEntry(e sqlExec, entry ledgerEntry) error {
	var price sql.NullString
	if entry.Price != nil {
		raw, err := json.Marshal(entry.Price)
		if err != nil {
			return err
		}
		price = sql.NullString{String: string(raw), Valid: true}
	}
	var upstream sql.NullFloat64
	if entry.UpstreamCostUSD != nil {
		upstream = sql.NullFloat64{Float64: *entry.UpstreamCostUSD, Valid: true}
	}
	_, err := e.exec("INSERT INTO ledger ("+ledgerColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		entry.ID, toNanos(entry.At), entry.UserID, entry.Username, entry.SessionID, entry.TurnID, entry.ResponseID,
		entry.Provider, entry.Model, entry.ResponseModel, entry.Kind, entry.Status, entry.KeySource, entry.BilledTo,
		entry.Input, entry.CacheRead, entry.CacheWrite, entry.Output, entry.Reasoning,
		price, boolInt(entry.Priced), entry.Cost, upstream, boolInt(entry.UsageAnomaly))
	if err != nil {
		return fmt.Errorf("write ledger: %w", err)
	}
	return nil
}

func scanLedgerEntry(rows *sql.Rows) (ledgerEntry, error) {
	var (
		e               ledgerEntry
		at              int64
		price           sql.NullString
		priced, anomaly int
		upstream        sql.NullFloat64
	)
	if err := rows.Scan(&e.ID, &at, &e.UserID, &e.Username, &e.SessionID, &e.TurnID, &e.ResponseID,
		&e.Provider, &e.Model, &e.ResponseModel, &e.Kind, &e.Status, &e.KeySource, &e.BilledTo,
		&e.Input, &e.CacheRead, &e.CacheWrite, &e.Output, &e.Reasoning,
		&price, &priced, &e.Cost, &upstream, &anomaly); err != nil {
		return e, err
	}
	e.At = fromNanos(at)
	if price.Valid {
		var snap priceSnapshot
		if err := json.Unmarshal([]byte(price.String), &snap); err == nil {
			e.Price = &snap
		}
	}
	e.Priced = priced != 0
	e.UsageAnomaly = anomaly != 0
	if upstream.Valid {
		v := upstream.Float64
		e.UpstreamCostUSD = &v
	}
	return e, nil
}

// ledgerQuery narrows a scan. Zero fields do not filter.
type ledgerQuery struct {
	From, To  time.Time // [From, To)
	UserID    string
	SessionID string
	Provider  string
	Model     string
}

// scan returns the entries matching q, oldest first.
func (l *ledgerStore) scan(q ledgerQuery) ([]ledgerEntry, error) {
	if l == nil {
		return nil, nil
	}
	var (
		where []string
		args  []any
	)
	if !q.From.IsZero() {
		where, args = append(where, "at >= ?"), append(args, toNanos(q.From))
	}
	if !q.To.IsZero() {
		where, args = append(where, "at < ?"), append(args, toNanos(q.To))
	}
	if q.UserID != "" {
		where, args = append(where, "user_id = ?"), append(args, q.UserID)
	}
	if q.SessionID != "" {
		where, args = append(where, "session_id = ?"), append(args, q.SessionID)
	}
	if q.Provider != "" {
		where, args = append(where, "provider = ?"), append(args, q.Provider)
	}
	if q.Model != "" {
		// A call counts under the model asked for and the model that answered.
		where, args = append(where, "(model = ? OR response_model = ?)"), append(args, q.Model, q.Model)
	}
	query := "SELECT " + ledgerColumns + " FROM ledger"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY at, id"
	var out []ledgerEntry
	err := l.db.query(query, func(rows *sql.Rows) error {
		e, err := scanLedgerEntry(rows)
		if err != nil {
			return err
		}
		out = append(out, e)
		return nil
	}, args...)
	return out, err
}

// The billing meter: a decorator around the provider stream that records every
// model call in the ledger.
//
// It wraps provider.StreamFn rather than hooking the agent loop because a user
// turn is not one model call. A turn with tool use makes several; a long
// session also makes a summary call to compact its context; a rate-limited call
// is re-issued. Every one of them goes through the StreamFn, so wrapping it is
// the one place that sees them all — and it needs no change to the loop.
//
// The per-turn facts the meter cannot know from a stream (whose turn it is,
// which key pays) travel in the context, set by runHostLoop. A call made with
// no turn in its context is passed through unmetered.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"sync"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/provider"
)

// turnInfo is what the meter knows about the user turn a call belongs to.
type turnInfo struct {
	userID    string
	username  string
	sessionID string
	turnID    string
	// keySource is where the provider key came from: "user" or "public"
	// (see credentialSource). Entries written before keys stopped coming from
	// the environment may also say "env".
	keySource string
	// emit reports each call's cost to the client as it finishes; nil when
	// the request is not streamed.
	emit func(streamEvent)

	// pending counts calls whose outcome is not yet recorded, so the turn can
	// wait for them before it reports that it is done.
	pending sync.WaitGroup

	// While holding, recorded entries are kept here instead of written: a
	// turn's calls are committed together, with the session's metadata, when
	// the turn ends (finishTurn). A process that crashes mid-turn loses them;
	// that is the accepted price of one commit per turn.
	mu      sync.Mutex
	holding bool
	held    []ledgerEntry
}

// hold starts keeping the turn's entries for its end.
func (t *turnInfo) hold() {
	t.mu.Lock()
	t.holding = true
	t.mu.Unlock()
}

// keep takes an entry while holding, reporting false otherwise (the caller
// then writes it itself).
func (t *turnInfo) keep(e ledgerEntry) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.holding {
		return false
	}
	t.held = append(t.held, e)
	return true
}

// release stops holding and hands over what was kept. An entry recorded
// afterwards — a call that outlived the turn's wait — is written directly.
func (t *turnInfo) release() []ledgerEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.holding = false
	out := t.held
	t.held = nil
	return out
}

type turnInfoKey struct{}

func withTurn(ctx context.Context, turn *turnInfo) context.Context {
	return context.WithValue(ctx, turnInfoKey{}, turn)
}

func turnFrom(ctx context.Context) *turnInfo {
	turn, _ := ctx.Value(turnInfoKey{}).(*turnInfo)
	return turn
}

// wait blocks until every call of the turn is recorded, or the limit passes.
// The limit is a guard, not an expected path: a call's stream ends promptly
// both when it completes and when its context is cancelled.
func (t *turnInfo) wait(limit time.Duration) {
	done := make(chan struct{})
	go func() {
		t.pending.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(limit):
		log.Printf("pigo-server: turn %s: usage recording still pending after %s", t.turnID, limit)
	}
}

// usageReport is the "usage" stream event's payload: one model call's tokens
// and cost, sent as the call finishes.
type usageReport struct {
	Kind       string  `json:"kind"`
	Status     string  `json:"status"`
	Provider   string  `json:"provider"`
	Model      string  `json:"model"`
	ResponseID string  `json:"responseId,omitempty"`
	Input      int     `json:"input"`
	CacheRead  int     `json:"cacheRead"`
	CacheWrite int     `json:"cacheWrite"`
	Output     int     `json:"output"`
	Reasoning  int     `json:"reasoning,omitempty"`
	Priced     bool    `json:"priced"`
	Cost       float64 `json:"cost"` // yuan
	BilledTo   string  `json:"billedTo"`
}

func reportOf(e ledgerEntry) *usageReport {
	model := e.ResponseModel
	if model == "" {
		model = e.Model
	}
	return &usageReport{
		Kind: e.Kind, Status: e.Status, Provider: e.Provider, Model: model,
		ResponseID: e.ResponseID,
		Input:      e.Input, CacheRead: e.CacheRead, CacheWrite: e.CacheWrite,
		Output: e.Output, Reasoning: e.Reasoning,
		Priced: e.Priced, Cost: yuan(e.Cost), BilledTo: e.BilledTo,
	}
}

// meter records calls into the ledger at the configured prices.
type meter struct {
	ledger   *ledgerStore
	settings *settingsStore
	now      func() time.Time
}

// wrap decorates a provider stream. kind labels the calls ("chat" or
// "compaction").
func (m *meter) wrap(inner provider.StreamFn, providerName, kind string) provider.StreamFn {
	if m == nil || inner == nil {
		return inner
	}
	return func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		in, err := inner(ctx, model, llm, cfg)
		turn := turnFrom(ctx)
		if err != nil || in == nil || turn == nil {
			return in, err
		}
		out := provider.NewAssistantMessageEventStream(0)
		turn.pending.Add(1)
		go m.relay(ctx, in, out, turn, call{provider: providerName, model: model, kind: kind})
		return out, nil
	}
}

// call is what the meter knows about one call before it starts.
type call struct {
	provider string
	model    string
	kind     string
}

// relay forwards the inner stream unchanged and records the call once it ends.
// The call is recorded before the outer stream closes, so by the time the loop
// sees the call finish, its cost is in the ledger.
//
// If the consumer stops reading (its context is cancelled), relay keeps
// draining the inner stream anyway: that is how it learns the call's outcome,
// and it keeps the provider's goroutine from blocking.
func (m *meter) relay(ctx context.Context, in, out *provider.AssistantMessageEventStream, turn *turnInfo, c call) {
	defer turn.pending.Done()

	var (
		last     agentcore.AssistantMessage
		started  bool // output began, so tokens were spent
		terminal string
		forward  = true
	)
	for ev := range in.Events() {
		switch e := ev.(type) {
		case provider.StreamTextEvent:
			last, started = e.Partial, true
		case provider.StreamThinkingEvent:
			last, started = e.Partial, true
		case provider.StreamToolCallEvent:
			last, started = e.Partial, true
		case provider.StreamDoneEvent:
			last, terminal = e.Message, usageOK
		case provider.StreamErrorEvent:
			last, terminal = e.Message, usageError
			if e.Message.StopReason == agentcore.StopReasonAborted || ctx.Err() != nil {
				terminal = usageAborted
			}
		}
		if forward && out.Emit(ctx, ev) != nil {
			forward = false
		}
	}
	if terminal == "" && ctx.Err() != nil {
		terminal = usageAborted
	}

	m.record(turn, c, last, terminal, started)

	// Carry the inner stream's outcome over for a producer that set it without
	// a terminal event; when one was forwarded, the outer result is already set
	// and this is a no-op.
	if res, err := in.Result(context.Background()); err != nil {
		out.SetError(err)
	} else {
		out.SetResult(res)
	}
	out.Close()
}

// record writes one call to the ledger, when there is anything to record:
//
//   - usage reported: always, whatever the status — the provider charged it;
//   - no usage, but output had started: an entry with no counts, so the
//     unmeasured spend is visible instead of silently missing — "unreported"
//     when the call finished (the provider sends no accounting), "aborted"
//     when it was cut off;
//   - no usage and no output (a rejected request, a rate limit): nothing.
func (m *meter) record(turn *turnInfo, c call, msg agentcore.AssistantMessage, status string, started bool) {
	var usage agentcore.Usage
	if msg.Usage != nil {
		usage = *msg.Usage
	}
	switch {
	case !usage.IsZero():
		if status == "" {
			status = usageAborted
		}
	case started && status == usageOK:
		status = usageUnreported
	case started:
		status = usageAborted
	default:
		return
	}

	entry := ledgerEntry{
		ID:              newLedgerID(),
		At:              m.clock(),
		UserID:          turn.userID,
		Username:        turn.username,
		SessionID:       turn.sessionID,
		TurnID:          turn.turnID,
		ResponseID:      msg.ResponseID,
		Provider:        c.provider,
		Model:           c.model,
		ResponseModel:   msg.ResponseModel,
		Kind:            c.kind,
		Status:          status,
		KeySource:       turn.keySource,
		BilledTo:        billedToFor(turn.keySource),
		Input:           usage.InputTokens,
		CacheRead:       usage.CacheReadTokens,
		CacheWrite:      usage.CacheWriteTokens,
		Output:          usage.OutputTokens,
		Reasoning:       usage.ReasoningTokens,
		UpstreamCostUSD: usage.UpstreamCostUSD,
		UsageAnomaly:    usage.Anomaly,
	}
	if price, ok := m.settings.priceFor(c.provider, msg.ResponseModel, c.model); ok {
		entry.Price = &price
		entry.Priced = true
		entry.Cost = rate(usage, price)
	}
	if turn.keep(entry) {
		// Committed when the turn ends.
	} else if err := m.ledger.append(entry); err != nil {
		// A failed audit write must not fail the user's turn; it is logged
		// with enough detail to reconstruct the entry.
		log.Printf("pigo-server: ledger write failed: %v (user %s session %s model %s in %d cache %d/%d out %d)",
			err, entry.UserID, shortID(entry.SessionID), entry.Model,
			entry.Input, entry.CacheRead, entry.CacheWrite, entry.Output)
	}
	if turn.emit != nil {
		turn.emit(streamEvent{Type: "usage", Usage: reportOf(entry)})
	}
}

func (m *meter) clock() time.Time {
	if m.now != nil {
		return m.now().UTC()
	}
	return time.Now().UTC()
}

func newLedgerID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

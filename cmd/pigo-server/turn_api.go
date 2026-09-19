// HTTP endpoints and session plumbing for turns (see turnrun.go).
package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"
)

// turnRetention is how long a finished turn stays attachable, so a client that
// reconnects just after the end still receives how it ended.
const turnRetention = 10 * time.Minute

// heartbeatEvery keeps an attached connection from looking dead to a proxy
// while a long tool runs, and gives the page something to show.
const heartbeatEvery = 20 * time.Second

// errTurnBusy is returned for a message sent while a turn is running.
var errTurnBusy = errors.New("上一轮还在运行：请等待它完成，或先点击停止")

// activeTurn is the session's running turn, or nil. Caller holds managed.mu.
func (m *managedSession) activeTurn() *turnRun {
	if m.turn != nil && m.turn.running() {
		return m.turn
	}
	return nil
}

func (s *apiServer) turnLimits() turnLimits {
	return turnLimits{
		idle:     s.config.turnIdle,
		max:      s.config.turnMax,
		maxSteps: s.config.turnMaxSteps,
		tick:     s.config.turnTick,
	}
}

// startTurn starts a turn for prompt in the background and subscribes the
// caller before it starts, so the caller sees every event. Caller holds
// managed.mu.
func (s *apiServer) startTurn(managed *managedSession, prompt, prefix string) (*turnRun, streamEvent, *turnSub, error) {
	if managed.activeTurn() != nil {
		return nil, streamEvent{}, nil, errTurnBusy
	}
	run := newTurnRun(managed.meta.ID, s.turnLimits())
	run.onAlive = func() {
		managed.mu.Lock()
		managed.meta.LastUsed = time.Now().UTC()
		managed.mu.Unlock()
	}
	managed.turn = run
	managed.meta.ActiveTurn = &turnRecord{ID: run.id, StartedAt: run.startedAt.UTC(), Status: "running"}
	managed.meta.LastUsed = time.Now().UTC()
	_ = s.saveSession(managed.meta)
	snapshot, sub, _ := run.subscribe()

	go run.watch()
	go func() {
		if prefix != "" {
			run.publish(streamEvent{Type: "delta", Text: prefix + "\n\n"})
		}
		reply, err := s.runMessage(withRun(run.ctx, run), managed, prompt, run.publish)
		s.finishTurn(managed, run, prefixText(prefix, reply), err)
	}()
	return run, snapshot, sub, nil
}

// finishTurn records the turn's end in the session and tells its clients.
func (s *apiServer) finishTurn(managed *managedSession, run *turnRun, reply string, runErr error) {
	final, record := run.outcome(reply, runErr)
	usage := run.takeLedger()
	managed.mu.Lock()
	if managed.meta.ActiveTurn != nil && managed.meta.ActiveTurn.ID == run.id {
		managed.meta.ActiveTurn = nil
	}
	rec := record
	managed.meta.LastTurn = &rec
	managed.meta.LastUsed = time.Now().UTC()
	meta, closed := managed.meta, managed.closed
	managed.mu.Unlock()
	// One commit for the turn: its usage, and the session's metadata. A
	// deleted session's usage is still recorded; the ledger outlives it.
	if s.db != nil && (len(usage) > 0 || !closed) {
		if err := s.db.inTx(func(tx *sqlTx) error {
			if !closed {
				if err := upsertSession(tx, meta); err != nil {
					return err
				}
			}
			for _, e := range usage {
				if err := insertLedgerEntry(tx, e); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			log.Printf("pigo-server: session %s: turn %s: saving %d usage records and the session failed: %v",
				shortID(run.sessionID), shortID(run.id), len(usage), err)
		}
	}
	run.finish(final, record)

	if record.Reason != turnDone {
		log.Printf("pigo-server: session %s: turn %s ended: %s after %d steps, %s: %s",
			shortID(run.sessionID), shortID(run.id), record.Reason, record.Steps,
			time.Since(run.startedAt).Round(time.Second), record.Message)
	}
	time.AfterFunc(turnRetention, func() {
		managed.mu.Lock()
		if managed.turn == run {
			managed.turn = nil
		}
		managed.mu.Unlock()
	})
}

// streamTurn writes a subscription to the response as NDJSON: the snapshot,
// then events as they happen, with a heartbeat while nothing does. It returns
// when the turn ends or the client goes away — which only detaches the client.
func (s *apiServer) streamTurn(w http.ResponseWriter, r *http.Request, run *turnRun, snapshot streamEvent, sub *turnSub, final *streamEvent) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		run.unsubscribe(sub)
		writeError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	encoder := json.NewEncoder(w)
	write := func(ev streamEvent) bool {
		if err := encoder.Encode(ev); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !write(snapshot) {
		run.unsubscribe(sub)
		return
	}
	if sub == nil {
		if final != nil {
			write(*final)
		}
		return
	}
	defer run.unsubscribe(sub)
	// Say at once what the turn is doing: a client attaching mid-command
	// would otherwise show nothing until the first heartbeat.
	if !write(run.heartbeat()) {
		return
	}
	ticker := time.NewTicker(heartbeatEvery)
	defer ticker.Stop()
	for {
		select {
		case ev, ok := <-sub.ch:
			if !ok {
				return
			}
			if !write(ev) {
				return
			}
			if ev.Type == "done" || ev.Type == "error" {
				return
			}
			ticker.Reset(heartbeatEvery)
		case <-ticker.C:
			if !write(run.heartbeat()) {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// waitTurn serves a non-streaming request: it waits for the turn and answers
// with the reply. A client that gives up does not stop the turn.
func (s *apiServer) waitTurn(w http.ResponseWriter, r *http.Request, run *turnRun, sub *turnSub) {
	defer run.unsubscribe(sub)
	var final *streamEvent
	for final == nil {
		select {
		case ev, ok := <-sub.ch:
			if !ok {
				// Dropped as too slow (it never reads): the turn still ends.
				select {
				case <-run.done:
				case <-r.Context().Done():
					return
				}
				_, _, final = run.subscribe()
				continue
			}
			if ev.Type == "done" || ev.Type == "error" {
				e := ev
				final = &e
			}
		case <-r.Context().Done():
			return
		}
	}
	if final.Type == "error" {
		writeError(w, statusForTurn(final.Reason), final.Error)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"reply": final.Text})
}

func statusForTurn(reason string) int {
	switch reason {
	case turnStalled, turnTimeLimit:
		return http.StatusGatewayTimeout
	case turnCanceled, turnShutdown, turnSessionClosed:
		return http.StatusConflict
	}
	return http.StatusBadGateway
}

// handleAttachTurn reattaches to the session's current turn (running, or
// finished within turnRetention).
func (s *apiServer) handleAttachTurn(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.sessionForRequest(r, r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	managed.mu.Lock()
	run := managed.turn
	managed.mu.Unlock()
	if run == nil {
		writeError(w, http.StatusNotFound, "no turn")
		return
	}
	snapshot, sub, final := run.subscribe()
	s.streamTurn(w, r, run, snapshot, sub, final)
}

// handleCancelTurn stops the session's running turn. The turn ends through
// its normal path, so attached clients receive the final event.
func (s *apiServer) handleCancelTurn(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.sessionForRequest(r, r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	managed.mu.Lock()
	run := managed.activeTurn()
	managed.mu.Unlock()
	if run == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "idle"})
		return
	}
	run.stop(errTurnCanceled)
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopping"})
}

// closeTurn stops a session's running turn because the session is going away
// (deleted, its owner disabled, or the process exiting), and waits a little for
// it to save what it has. Caller does not hold managed.mu.
func (s *apiServer) closeTurn(managed *managedSession, cause error) {
	managed.mu.Lock()
	run := managed.activeTurn()
	managed.mu.Unlock()
	if run == nil {
		return
	}
	run.stop(cause)
	if !run.wait(10 * time.Second) {
		log.Printf("pigo-server: session %s: turn %s did not stop within 10s", shortID(run.sessionID), shortID(run.id))
	}
}

// stopAllTurns stops every running turn for a shutdown and waits, up to limit
// in total, for them to save.
func (s *apiServer) stopAllTurns(limit time.Duration) {
	s.mu.RLock()
	var runs []*turnRun
	for _, managed := range s.sessions {
		managed.mu.Lock()
		if run := managed.activeTurn(); run != nil {
			runs = append(runs, run)
		}
		managed.mu.Unlock()
	}
	s.mu.RUnlock()
	for _, run := range runs {
		run.stop(errTurnShutdown)
	}
	deadline := time.Now().Add(limit)
	for _, run := range runs {
		run.wait(time.Until(deadline))
	}
	if len(runs) > 0 {
		log.Printf("pigo-server: stopped %d running turn(s) for shutdown", len(runs))
	}
}

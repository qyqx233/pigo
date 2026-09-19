// A turn — one user message and everything the agent does about it — runs on
// the server independently of the HTTP request that started it.
//
// A long task makes dozens of model and tool calls. Tying it to the request
// meant a page reload, a laptop lid or a proxy timeout killed the work, and a
// fixed ten-minute request deadline cut off any task that took longer. Here the
// request only starts the turn and subscribes to it; closing the connection
// unsubscribes. The turn ends when the agent finishes, when the user stops it,
// or when one of its brakes trips:
//
//   - no progress for the idle limit (the default brake: a working agent is
//     never stopped for being slow, only for being stuck);
//   - the overall time limit, a generous backstop;
//   - the step limit, which asks the model to sum up rather than cutting it off.
//
// A turn keeps a materialized view of itself — the reply text so far, the cost
// of each model call, what it is doing now — rather than a log of events. A
// client that (re)attaches gets that view as one snapshot and the live events
// after it, so a reconnect never replays or misses anything and the memory a
// turn holds does not grow with the number of events.
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/runtime"
)

// Why a turn ended.
const (
	turnDone          = "done"
	turnFailed        = "error"
	turnStalled       = "stalled"
	turnTimeLimit     = "time_limit"
	turnStepLimit     = "step_limit"
	turnCanceled      = "canceled"
	turnShutdown      = "shutdown"
	turnSessionClosed = "session_closed"
	// turnInterrupted is recorded at startup for a turn the previous process
	// never finished (it crashed or was killed without a clean shutdown).
	turnInterrupted = "interrupted"
)

// turnStop is the cancellation cause of a turn stopped from outside the agent.
type turnStop struct{ reason string }

func (e *turnStop) Error() string { return "turn stopped: " + e.reason }

var (
	errTurnStalled       = &turnStop{turnStalled}
	errTurnTimeLimit     = &turnStop{turnTimeLimit}
	errTurnCanceled      = &turnStop{turnCanceled}
	errTurnShutdown      = &turnStop{turnShutdown}
	errTurnSessionClosed = &turnStop{turnSessionClosed}
)

// stopReason reports why ctx's turn was stopped, or "" if it was not.
func stopReason(ctx context.Context) string {
	if ctx.Err() == nil {
		return ""
	}
	var stop *turnStop
	if errors.As(context.Cause(ctx), &stop) {
		return stop.reason
	}
	return turnCanceled
}

// turnLimits are the brakes. A zero field disables that brake.
type turnLimits struct {
	// idle is how long a turn may go without progress.
	idle time.Duration
	// max is the overall time limit.
	max time.Duration
	// maxSteps is how many tool calls a turn may make before it is asked to
	// wrap up.
	maxSteps int
	// toolGrace is the idle limit while a tool is running: a tool reports
	// nothing until it finishes, and must not be mistaken for a stall before
	// its own timeout has had a chance to fire.
	toolGrace time.Duration
	// tick is how often the brakes are checked.
	tick time.Duration
}

// sandboxBashMax is the longest a single bash command may run (sandboxbash.go);
// a turn waiting on one is not stalled until that has passed.
const sandboxBashMax = 10 * time.Minute

func defaultToolGrace() time.Duration { return sandboxBashMax + time.Minute }

// turnRecord is a turn as persisted in the session's meta.json: the running
// turn (so a restart can tell it never finished) and the last finished one (so
// a reopened page can say how it ended).
type turnRecord struct {
	ID        string     `json:"id"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	Status    string     `json:"status"`
	Reason    string     `json:"reason,omitempty"`
	Steps     int        `json:"steps"`
	// Message explains a turn that did not end normally, in the words shown
	// to the user.
	Message string `json:"message,omitempty"`
}

// turnSub is one attached client.
type turnSub struct {
	ch chan streamEvent
}

// subscriberBuffer bounds what a slow client may fall behind by. A client that
// falls further is dropped and, reconnecting, gets a fresh snapshot.
const subscriberBuffer = 256

type turnRun struct {
	id        string
	sessionID string
	startedAt time.Time
	limits    turnLimits

	ctx    context.Context
	cancel context.CancelCauseFunc
	done   chan struct{}

	// onAlive is called (at most every aliveEvery) while the turn is making
	// progress, so the session stays fresh for the idle reaper.
	onAlive func()

	mu sync.Mutex
	// text is the model's text since the last tool call: the answer, if the
	// turn ends here, or narration that moves into activity when a tool
	// call follows.
	text         strings.Builder
	activity     []activityItem
	usage        []*usageReport
	steps        int
	toolsRunning int
	tool         string
	phaseSince   time.Time
	lastProgress time.Time
	lastAlive    time.Time
	final        *streamEvent
	record       turnRecord
	// ledger is the turn's usage, committed when it ends.
	ledger []ledgerEntry
	subs   map[*turnSub]struct{}

	// transcriptStart is where this turn's user message sits in the
	// transcript file; history hides what follows it while the turn runs,
	// because the snapshot carries it. The file only grows (a compaction is
	// appended), so the position holds — unless the file was rewritten, after
	// which it means nothing and nothing is hidden.
	transcriptStart int
	rewritten       bool

	// Step counting and the step-limit wrap-up, touched only from the agent
	// loop's goroutine (see installHooks). counted is how much of the message
	// list has been counted.
	counted       int
	wrapUpPending bool
	wrapUpArmed   bool
	stepLimited   bool
}

const aliveEvery = 30 * time.Second

func newTurnRun(sessionID string, limits turnLimits) *turnRun {
	if limits.toolGrace <= 0 {
		limits.toolGrace = defaultToolGrace()
	}
	if limits.tick <= 0 {
		limits.tick = 5 * time.Second
	}
	now := time.Now()
	ctx, cancel := context.WithCancelCause(context.Background())
	return &turnRun{
		id:              newLedgerID(),
		sessionID:       sessionID,
		startedAt:       now,
		limits:          limits,
		ctx:             ctx,
		cancel:          cancel,
		done:            make(chan struct{}),
		phaseSince:      now,
		lastProgress:    now,
		lastAlive:       now,
		subs:            map[*turnSub]struct{}{},
		transcriptStart: -1,
	}
}

type turnRunKey struct{}

func withRun(ctx context.Context, run *turnRun) context.Context {
	return context.WithValue(ctx, turnRunKey{}, run)
}

func runFrom(ctx context.Context) *turnRun {
	run, _ := ctx.Value(turnRunKey{}).(*turnRun)
	return run
}

// running reports whether the turn has not finished.
func (r *turnRun) running() bool {
	if r == nil {
		return false
	}
	select {
	case <-r.done:
		return false
	default:
		return true
	}
}

// stop cancels the turn with cause. A finished turn ignores it.
func (r *turnRun) stop(cause error) {
	if r != nil {
		r.cancel(cause)
	}
}

// wait blocks until the turn finishes or limit passes.
func (r *turnRun) wait(limit time.Duration) bool {
	if r == nil {
		return true
	}
	select {
	case <-r.done:
		return true
	case <-time.After(limit):
		return false
	}
}

// --- state updates -------------------------------------------------------------

// publish is the turn's emit function: it folds an event into the snapshot and
// hands it to every attached client. It is called from the agent's event
// drain and from the billing meter, so it locks.
func (r *turnRun) publish(ev streamEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.final != nil {
		return
	}
	switch ev.Type {
	case "delta":
		r.text.WriteString(ev.Text)
	case "usage":
		if ev.Usage != nil {
			r.usage = append(r.usage, ev.Usage)
		}
	case "tool":
		r.foldToolLocked(ev)
	case "compaction":
		r.foldCompactionLocked(ev)
	}
	r.broadcastLocked(ev)
}

// foldToolLocked records a tool event in the activity log. The text the model
// wrote before a call is its narration of that step, so it moves into the log.
func (r *turnRun) foldToolLocked(ev streamEvent) {
	find := func() *activityItem {
		for i := len(r.activity) - 1; i >= 0; i-- {
			if r.activity[i].Kind == "tool" && r.activity[i].ID == ev.ID {
				return &r.activity[i]
			}
		}
		return nil
	}
	switch ev.Phase {
	case "start":
		if narration := strings.TrimSpace(r.text.String()); narration != "" {
			r.activity = append(r.activity, activityItem{Kind: "text", Text: narration})
		}
		r.text.Reset()
		r.activity = append(r.activity, activityItem{Kind: "tool", Tool: ev.Tool, ID: ev.ID, Detail: ev.Detail, Status: "running"})
	case "output":
		if item := find(); item != nil {
			item.Output = ev.Text
		}
	case "end":
		status := "ok"
		if ev.IsError {
			status = "error"
		}
		item := find()
		if item == nil {
			// A call the loop rejected before running it has no start.
			if narration := strings.TrimSpace(r.text.String()); narration != "" {
				r.activity = append(r.activity, activityItem{Kind: "text", Text: narration})
			}
			r.text.Reset()
			r.activity = append(r.activity, activityItem{Kind: "tool", Tool: ev.Tool, ID: ev.ID})
			item = &r.activity[len(r.activity)-1]
		}
		item.Status, item.Text, item.ElapsedMs, item.Output = status, ev.Text, ev.ElapsedMs, ""
	}
}

// foldCompactionLocked records a context compaction in the activity log. The
// text written before it — often the turn's answer, since compaction runs after
// the last call — moves into the log first, so the note follows it.
func (r *turnRun) foldCompactionLocked(ev streamEvent) {
	switch ev.Phase {
	case "start":
		if narration := strings.TrimSpace(r.text.String()); narration != "" {
			r.activity = append(r.activity, activityItem{Kind: "text", Text: narration})
		}
		r.text.Reset()
		r.activity = append(r.activity, activityItem{Kind: "compaction", Status: "running", Compaction: ev.Compaction})
	case "end":
		status := "ok"
		if ev.IsError {
			status = "error"
		}
		for i := len(r.activity) - 1; i >= 0; i-- {
			if r.activity[i].Kind == "compaction" && r.activity[i].Status == "running" {
				if skippedCompaction(ev) {
					// Nothing was summarized: nothing to show.
					r.activity = append(r.activity[:i], r.activity[i+1:]...)
					return
				}
				r.activity[i].Status, r.activity[i].Text, r.activity[i].Compaction = status, ev.Error, ev.Compaction
				return
			}
		}
		r.activity = append(r.activity, activityItem{Kind: "compaction", Status: status, Text: ev.Error, Compaction: ev.Compaction})
	}
}

func (r *turnRun) broadcastLocked(ev streamEvent) {
	for sub := range r.subs {
		select {
		case sub.ch <- ev:
		default:
			// Too far behind: drop it rather than stall the turn. The client
			// sees its stream end without a final event and reattaches.
			close(sub.ch)
			delete(r.subs, sub)
		}
	}
}

// observe records progress from the agent's own events: every event counts as
// the turn being alive, and tool starts and ends move the phase.
func (r *turnRun) observe(ev agentcore.AgentEvent) {
	now := time.Now()
	r.mu.Lock()
	r.lastProgress = now
	switch e := ev.(type) {
	case agentcore.ToolExecutionStartEvent:
		r.toolsRunning++
		r.tool = e.ToolName
		r.phaseSince = now
	case agentcore.ToolExecutionEndEvent:
		if r.toolsRunning > 0 {
			r.toolsRunning--
		}
		if r.toolsRunning == 0 {
			r.tool = ""
			r.phaseSince = now
		}
	}
	alive := r.onAlive != nil && now.Sub(r.lastAlive) >= aliveEvery
	if alive {
		r.lastAlive = now
	}
	r.mu.Unlock()
	if alive {
		r.onAlive()
	}
}

// addLedger keeps usage entries for the turn's end.
func (r *turnRun) addLedger(entries []ledgerEntry) {
	r.mu.Lock()
	r.ledger = append(r.ledger, entries...)
	r.mu.Unlock()
}

// takeLedger hands over the kept entries.
func (r *turnRun) takeLedger() []ledgerEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.ledger
	r.ledger = nil
	return out
}

func (r *turnRun) stepCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.steps
}

// transcriptRewritten notes that the transcript file was rewritten, so the
// position recorded by setTranscriptStart no longer holds.
func (r *turnRun) transcriptRewritten() {
	r.mu.Lock()
	r.rewritten = true
	r.mu.Unlock()
}

// setTranscriptStart records where the turn's user message sits.
func (r *turnRun) setTranscriptStart(index int) {
	r.mu.Lock()
	r.transcriptStart = index
	r.mu.Unlock()
}

// historyCut is how many transcript entries history should show while the
// turn runs (the entries up to and including its user message), or -1 to show
// everything.
func (r *turnRun) historyCut() int {
	if !r.running() {
		return -1
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rewritten || r.transcriptStart < 0 {
		return -1
	}
	return r.transcriptStart + 1
}

// --- attaching -------------------------------------------------------------------

// subscribe attaches a client. It returns the snapshot to send first and, while
// the turn runs, the subscription carrying everything after it; for a finished
// turn the subscription is nil and final is the event that ended it. Taking
// the snapshot and registering happen under one lock, so nothing falls between
// them.
func (r *turnRun) subscribe() (snapshot streamEvent, sub *turnSub, final *streamEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot = streamEvent{
		Type:      "snapshot",
		Text:      r.text.String(),
		Activity:  append([]activityItem(nil), r.activity...),
		Usages:    append([]*usageReport(nil), r.usage...),
		Steps:     r.steps,
		ID:        r.id,
		StartedAt: timePtr(r.startedAt),
	}
	if r.final != nil {
		return snapshot, nil, r.final
	}
	sub = &turnSub{ch: make(chan streamEvent, subscriberBuffer)}
	r.subs[sub] = struct{}{}
	return snapshot, sub, nil
}

func (r *turnRun) unsubscribe(sub *turnSub) {
	if sub == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.subs[sub]; ok {
		delete(r.subs, sub)
		close(sub.ch)
	}
}

// heartbeat describes what the turn is doing now. It is sent to an idle
// connection so a client can show progress and proxies keep it open; it is
// not progress itself.
func (r *turnRun) heartbeat() streamEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	ev := streamEvent{Type: "heartbeat", Steps: r.steps, ElapsedMs: time.Since(r.phaseSince).Milliseconds()}
	if r.toolsRunning > 0 {
		ev.Phase, ev.Tool = "tool", r.tool
	} else {
		ev.Phase = "model"
	}
	return ev
}

// info is the turn as the session endpoint reports it.
func (r *turnRun) info() turnRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.final != nil {
		return r.record
	}
	return turnRecord{ID: r.id, StartedAt: r.startedAt, Status: "running", Steps: r.steps}
}

// --- ending ------------------------------------------------------------------------

// finish records how the turn ended, sends the final event to every client and
// detaches them.
func (r *turnRun) finish(final streamEvent, record turnRecord) {
	r.mu.Lock()
	if r.final != nil {
		r.mu.Unlock()
		return
	}
	r.final = &final
	r.record = record
	for sub := range r.subs {
		select {
		case sub.ch <- final:
		default:
		}
		close(sub.ch)
		delete(r.subs, sub)
	}
	r.mu.Unlock()
	r.cancel(nil)
	close(r.done)
}

// outcome turns what the agent returned into the turn's ending: the final
// event for clients and the record for meta.json.
func (r *turnRun) outcome(reply string, runErr error) (streamEvent, turnRecord) {
	steps := r.stepCount()
	now := time.Now().UTC()
	record := turnRecord{ID: r.id, StartedAt: r.startedAt.UTC(), EndedAt: &now, Steps: steps}

	reason := stopReason(r.ctx)
	switch {
	case reason != "":
		// Stopped from outside: whatever error the agent reported is just the
		// cancellation arriving.
	case runErr != nil:
		reason = turnFailed
	case r.stepLimited:
		reason = turnStepLimit
	default:
		reason = turnDone
	}
	record.Reason = reason
	message := turnMessage(reason, steps, r.limits)
	if reason == turnFailed {
		message = runErr.Error()
	}

	switch reason {
	case turnDone, turnStepLimit:
		record.Status = "done"
		record.Message = message
		return streamEvent{Type: "done", Text: reply, Reason: reason, Steps: steps, Detail: message}, record
	default:
		record.Status = "stopped"
		if reason == turnFailed {
			record.Status = "error"
		}
		record.Message = message
		return streamEvent{Type: "error", Error: message, Reason: reason, Steps: steps}, record
	}
}

// turnMessage is what the user is told about a turn that did not simply
// finish. Every stop says what was kept and what to do next.
func turnMessage(reason string, steps int, limits turnLimits) string {
	kept := fmt.Sprintf("已完成 %d 步，结果已保存", steps)
	switch reason {
	case turnStalled:
		return fmt.Sprintf("本轮已中止：超过 %s没有任何进展。%s，可以发送“继续”接着做。", humanDuration(limits.idle), kept)
	case turnTimeLimit:
		return fmt.Sprintf("本轮已中止：运行超过 %s上限。%s，可以发送“继续”接着做。", humanDuration(limits.max), kept)
	case turnStepLimit:
		return fmt.Sprintf("本轮已达 %d 步上限，模型已总结进度。发送“继续”可以接着做。", limits.maxSteps)
	case turnCanceled:
		return fmt.Sprintf("已停止。%s。", kept)
	case turnShutdown:
		return fmt.Sprintf("服务正在重启，本轮中断。%s，重启后可以发送“继续”。", kept)
	case turnInterrupted:
		// Steps are not written until a turn ends, so their number is unknown.
		return "上一轮因服务意外退出而中断。已完成的步骤保存在对话里，可以发送“继续”接着做。"
	case turnSessionClosed:
		return "会话已关闭。"
	}
	return ""
}

func humanDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "—"
	case d%time.Hour == 0:
		return fmt.Sprintf("%d 小时", int(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("%d 分钟", int(d/time.Minute))
	default:
		return d.Round(time.Second).String()
	}
}

// --- brakes ------------------------------------------------------------------------

// watch enforces the idle and overall limits until the turn ends.
func (r *turnRun) watch() {
	if r.limits.idle <= 0 && r.limits.max <= 0 {
		return
	}
	ticker := time.NewTicker(r.limits.tick)
	defer ticker.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-r.ctx.Done():
			return
		case now := <-ticker.C:
			if cause := r.brake(now); cause != nil {
				r.cancel(cause)
				return
			}
		}
	}
}

// brake reports which limit, if any, the turn has passed at now.
func (r *turnRun) brake(now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.limits.max > 0 && now.Sub(r.startedAt) >= r.limits.max {
		return errTurnTimeLimit
	}
	if r.limits.idle > 0 {
		limit := r.limits.idle
		if r.toolsRunning > 0 && r.limits.toolGrace > limit {
			limit = r.limits.toolGrace
		}
		if now.Sub(r.lastProgress) >= limit {
			return errTurnStalled
		}
	}
	return nil
}

// stepLimitNote is the message that asks the model to wrap up. It is a user
// message so it stays in the transcript and a later "继续" has the context.
const stepLimitNote = "[系统] 本轮的工具调用已达 %d 次上限。请不要再调用工具：总结目前完成了什么、还剩哪些工作，然后停止。用户可以发送“继续”让你接着做。"

// installHooks attaches the turn's per-step hooks to the loop configuration.
// Both run on the agent loop's goroutine after every step, while the message
// list is stable — which is why steps are counted here and not from events:
// the loop does not wait for its events to be handled, so an event-driven
// count lags the loop by up to a step.
//
//   - PrepareNextTurn counts the step's tool results (a call the loop rejects,
//     an unknown tool, never starts but is a step all the same) and, at the
//     limit, appends the note asking the model to wrap up.
//   - ShouldStopAfterTurn saves the checkpoint and, one step after the note,
//     ends the turn.
//
// start is the length of the message list when the turn began. Existing hooks
// are kept.
func (r *turnRun) installHooks(cfg *runtime.RunConfig, start int, checkpoint func(*agentcore.AgentContext)) {
	r.counted = start
	prevPrepare := cfg.PrepareNextTurn
	cfg.PrepareNextTurn = func(ctx context.Context, agentCtx *agentcore.AgentContext) *runtime.TurnUpdate {
		var update *runtime.TurnUpdate
		if prevPrepare != nil {
			update = prevPrepare(ctx, agentCtx)
		}
		msgs := agentCtx.Messages
		if update != nil && update.Messages != nil {
			msgs = *update.Messages
		}
		if r.counted > len(msgs) {
			r.counted = len(msgs)
		}
		added := 0
		for _, m := range msgs[r.counted:] {
			if _, ok := m.(agentcore.ToolResultMessage); ok {
				added++
			}
		}
		r.counted = len(msgs)
		r.mu.Lock()
		r.steps += added
		steps := r.steps
		r.mu.Unlock()

		if r.limits.maxSteps > 0 && !r.wrapUpPending && !r.wrapUpArmed && steps >= r.limits.maxSteps {
			r.wrapUpPending = true
			next := append(append(agentcore.MessageList(nil), msgs...), agentcore.UserMessage{
				RoleField: agentcore.RoleUser,
				Content:   agentcore.ContentList{agentcore.NewTextContent(fmt.Sprintf(stepLimitNote, r.limits.maxSteps))},
			})
			r.counted = len(next)
			if update == nil {
				update = &runtime.TurnUpdate{}
			}
			update.Messages = &next
		}
		return update
	}
	prevStop := cfg.ShouldStopAfterTurn
	cfg.ShouldStopAfterTurn = func(ctx context.Context, agentCtx *agentcore.AgentContext) bool {
		// Compaction may have rewritten the list since the count.
		r.counted = len(agentCtx.Messages)
		if checkpoint != nil {
			checkpoint(agentCtx)
		}
		stop := prevStop != nil && prevStop(ctx, agentCtx)
		switch {
		case r.wrapUpPending:
			// The note was just added: give the model one more step to act on it.
			r.wrapUpPending = false
			r.wrapUpArmed = true
		case r.wrapUpArmed:
			r.stepLimited = true
			stop = true
		}
		return stop
	}
}

func timePtr(t time.Time) *time.Time {
	u := t.UTC()
	return &u
}

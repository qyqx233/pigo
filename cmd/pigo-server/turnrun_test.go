package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/runtime"
)

func TestTurnBrakes(t *testing.T) {
	run := newTurnRun("s", turnLimits{idle: 10 * time.Minute, max: 2 * time.Hour, toolGrace: 11 * time.Minute})
	start := run.startedAt

	if err := run.brake(start.Add(9 * time.Minute)); err != nil {
		t.Errorf("9m idle: %v", err)
	}
	if err := run.brake(start.Add(10 * time.Minute)); !errors.Is(err, errTurnStalled) {
		t.Errorf("10m idle: %v, want stalled", err)
	}

	// A running tool reports nothing until it finishes; the idle limit waits
	// for the tool's own timeout.
	run.observe(agentcore.ToolExecutionStartEvent{ToolName: "bash"})
	began := run.lastProgress
	if err := run.brake(began.Add(10 * time.Minute)); err != nil {
		t.Errorf("tool running 10m: %v", err)
	}
	if err := run.brake(began.Add(11 * time.Minute)); !errors.Is(err, errTurnStalled) {
		t.Errorf("tool running past its grace: %v", err)
	}
	run.observe(agentcore.ToolExecutionEndEvent{ToolName: "bash"})

	// Progress keeps a turn alive right up to the overall limit.
	run.mu.Lock()
	run.lastProgress = start.Add(2*time.Hour - time.Second)
	run.mu.Unlock()
	if err := run.brake(start.Add(2 * time.Hour)); !errors.Is(err, errTurnTimeLimit) {
		t.Errorf("2h: %v, want time limit", err)
	}

	if err := newTurnRun("s", turnLimits{}).brake(start.Add(100 * time.Hour)); err != nil {
		t.Errorf("no limits: %v", err)
	}
}

// TestTurnWatchStops: the watcher cancels the turn with the brake as cause.
func TestTurnWatchStops(t *testing.T) {
	run := newTurnRun("s", turnLimits{idle: 50 * time.Millisecond, tick: 10 * time.Millisecond})
	go run.watch()
	select {
	case <-run.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("stalled turn was not stopped")
	}
	if got := stopReason(run.ctx); got != turnStalled {
		t.Errorf("reason = %q", got)
	}
}

func TestTurnOutcome(t *testing.T) {
	limits := turnLimits{idle: 10 * time.Minute, max: 2 * time.Hour, maxSteps: 200}
	cases := []struct {
		name       string
		cause      error
		runErr     error
		stepLimit  bool
		wantType   string
		wantReason string
		wantText   string
	}{
		{"done", nil, nil, false, "done", turnDone, ""},
		{"step limit", nil, nil, true, "done", turnStepLimit, "200 步上限"},
		{"upstream error", nil, errors.New("upstream 500"), false, "error", turnFailed, "upstream 500"},
		// The agent reports the cancellation as its own error; the cause wins.
		{"stalled", errTurnStalled, context.Canceled, false, "error", turnStalled, "超过 10 分钟没有任何进展"},
		{"time limit", errTurnTimeLimit, context.Canceled, false, "error", turnTimeLimit, "2 小时"},
		{"canceled", errTurnCanceled, context.Canceled, false, "error", turnCanceled, "已停止"},
		{"shutdown", errTurnShutdown, context.Canceled, false, "error", turnShutdown, "重启"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := newTurnRun("s", limits)
			run.steps = 1
			run.stepLimited = tc.stepLimit
			if tc.cause != nil {
				run.stop(tc.cause)
			}
			final, record := run.outcome("reply", tc.runErr)
			text := final.Error + final.Detail
			if final.Type != tc.wantType || final.Reason != tc.wantReason || !strings.Contains(text, tc.wantText) {
				t.Errorf("final = %+v", final)
			}
			if record.Reason != tc.wantReason || record.Steps != 1 || record.EndedAt == nil {
				t.Errorf("record = %+v", record)
			}
			if tc.cause != nil && tc.cause != errTurnShutdown && !strings.Contains(text, "已完成 1 步") && tc.cause != errTurnCanceled {
				t.Errorf("a stopped turn must say what was kept: %q", text)
			}
		})
	}
}

// TestTurnSubscribe: a snapshot and the live events after it add up to exactly
// what was published, and a finished turn hands late subscribers its end.
func TestTurnSubscribe(t *testing.T) {
	run := newTurnRun("s", turnLimits{})
	run.publish(streamEvent{Type: "delta", Text: "Hello, "})
	run.publish(streamEvent{Type: "usage", Usage: &usageReport{Cost: 0.5}})

	snapshot, sub, final := run.subscribe()
	if final != nil || sub == nil {
		t.Fatal("running turn returned no subscription")
	}
	if snapshot.Type != "snapshot" || snapshot.Text != "Hello, " || len(snapshot.Usages) != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	run.publish(streamEvent{Type: "delta", Text: "world"})
	if ev := <-sub.ch; ev.Text != "world" {
		t.Fatalf("live event = %+v", ev)
	}

	run.finish(streamEvent{Type: "done", Text: "Hello, world"}, turnRecord{Status: "done"})
	if ev := <-sub.ch; ev.Type != "done" {
		t.Fatalf("final = %+v", ev)
	}
	if _, ok := <-sub.ch; ok {
		t.Error("subscription not closed after the end")
	}
	if run.running() {
		t.Error("finished turn still running")
	}
	// Publishing after the end changes nothing.
	run.publish(streamEvent{Type: "delta", Text: "late"})

	late, lateSub, lateFinal := run.subscribe()
	if lateSub != nil || lateFinal == nil || lateFinal.Type != "done" || late.Text != "Hello, world" {
		t.Errorf("late subscribe = %+v %v %+v", late, lateSub, lateFinal)
	}
}

// TestTurnDropsSlowSubscriber: a client that stops reading is dropped rather
// than holding the turn up.
func TestTurnDropsSlowSubscriber(t *testing.T) {
	run := newTurnRun("s", turnLimits{})
	_, sub, _ := run.subscribe()
	done := make(chan struct{})
	go func() {
		for i := 0; i < subscriberBuffer+10; i++ {
			run.publish(streamEvent{Type: "delta", Text: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish blocked on a slow subscriber")
	}
	n := 0
	for range sub.ch {
		n++
	}
	if n != subscriberBuffer {
		t.Errorf("slow subscriber received %d, want the buffer then a closed channel", n)
	}
}

// TestTurnStepLimitHooks walks the hooks in the loop's order after every step
// (prepare the next turn, then the stop check), with the message list growing
// as the loop grows it: the wrap-up note goes in once at the limit, the model
// gets one more step, then the turn stops. Steps are counted from the tool
// results in the list, so a rejected call counts too.
func TestTurnStepLimitHooks(t *testing.T) {
	run := newTurnRun("s", turnLimits{maxSteps: 2})
	var cfg runtime.RunConfig
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("go")}},
	}}
	checkpoints := 0
	run.installHooks(&cfg, len(agentCtx.Messages), func(*agentcore.AgentContext) { checkpoints++ })
	ctx := context.Background()
	step := func(tools int) (note bool, stop bool) {
		agentCtx.Messages = append(agentCtx.Messages, agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant})
		for i := 0; i < tools; i++ {
			agentCtx.Messages = append(agentCtx.Messages, agentcore.ToolResultMessage{RoleField: agentcore.RoleToolResult, IsError: i%2 == 1})
		}
		if update := cfg.PrepareNextTurn(ctx, agentCtx); update != nil && update.Messages != nil {
			agentCtx.Messages = *update.Messages
			note = true
		}
		return note, cfg.ShouldStopAfterTurn(ctx, agentCtx)
	}
	if note, stop := step(1); note || stop {
		t.Fatal("step 1 of 2")
	}
	note, stop := step(1)
	if !note || stop {
		t.Fatalf("at the limit: note %v stop %v; want the note and one more step", note, stop)
	}
	if last, ok := agentCtx.Messages[len(agentCtx.Messages)-1].(agentcore.UserMessage); !ok ||
		!strings.Contains(agentcore.ContentToText(last.Content), "2 次上限") {
		t.Fatalf("the note is not the last message: %+v", agentCtx.Messages[len(agentCtx.Messages)-1])
	}
	if note, stop := step(2); note || !stop {
		t.Fatalf("wrap-up step: note %v stop %v; want a stop without a second note", note, stop)
	}
	if !run.stepLimited || checkpoints != 3 || run.stepCount() != 4 {
		t.Errorf("stepLimited %v, checkpoints %d, steps %d", run.stepLimited, checkpoints, run.stepCount())
	}
}

func TestRecoverInterruptedTurn(t *testing.T) {
	meta := sessionMeta{ActiveTurn: &turnRecord{ID: "t1", StartedAt: time.Now(), Status: "running", Steps: 17}}
	if !recoverInterruptedTurn(&meta) {
		t.Fatal("not recovered")
	}
	if meta.ActiveTurn != nil || meta.LastTurn == nil || meta.LastTurn.Reason != turnInterrupted ||
		!strings.Contains(meta.LastTurn.Message, "保存在对话里") {
		t.Errorf("meta = %+v / %+v", meta.ActiveTurn, meta.LastTurn)
	}
	if recoverInterruptedTurn(&meta) {
		t.Error("recovered twice")
	}
}

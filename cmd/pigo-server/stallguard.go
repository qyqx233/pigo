// The stall guard: a decorator around the provider stream that abandons a model
// call which has gone quiet.
//
// The transport under the Chat Completions and Anthropic drivers already has an
// idle watchdog, but the Responses driver streams through the OpenAI SDK and
// has none — a hung upstream there holds the whole turn until the turn's own
// limit. Guarding at the StreamFn covers every driver the same way, without a
// change to the provider package.
//
// A call abandoned before it produced anything is reported as a transient
// failure, so the loop's retry policy re-issues it. One abandoned after output
// started cannot be replayed (the partial already reached the user) and ends
// the turn with the error, as any mid-stream failure does.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/provider"
)

// defaultStreamIdle matches the transport's own watchdog, and honours the same
// PIGO_STREAM_IDLE_TIMEOUT override so one setting governs both.
//
// defaultStreamFirstByte is the shorter deadline for a call that has produced
// nothing at all. A model that has not emitted its first token after two
// minutes is hung rather than slow, and since nothing reached the user the
// call can simply be re-issued.
const (
	defaultStreamIdle      = 5 * time.Minute
	defaultStreamFirstByte = 2 * time.Minute
)

func envDuration(name string, fallback time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}

func streamIdleTimeout() time.Duration {
	return envDuration("PIGO_STREAM_IDLE_TIMEOUT", defaultStreamIdle)
}

// streamFirstByteTimeout never exceeds the idle limit: a deployment that
// shortens the idle window means calls to be abandoned sooner, not later.
func streamFirstByteTimeout(idle time.Duration) time.Duration {
	d := envDuration("PIGO_STREAM_FIRST_BYTE_TIMEOUT", defaultStreamFirstByte)
	if d > idle {
		return idle
	}
	return d
}

// streamStallError reports an abandoned call. It classifies itself as
// transient for provider.IsTransient.
type streamStallError struct {
	idle time.Duration
	// silent is set when the call was abandoned before producing anything.
	silent bool
}

func (e *streamStallError) Error() string {
	if e.silent {
		return fmt.Sprintf("the model produced nothing within %s; the call was abandoned", e.idle)
	}
	return fmt.Sprintf("the model sent nothing for %s; the call was abandoned", e.idle)
}

func (e *streamStallError) Transient() bool { return true }

// guardStream decorates inner so a call that stops making progress is
// cancelled and ended with a streamStallError.
//
// firstByte is the deadline until the model produces anything; pass idle to
// waive it, as a compaction call does — it sends the whole history, so its
// first token is the slowest of any call, and losing it costs the turn.
//
// label names the call in the diagnostic line an abnormal end writes.
func guardStream(inner provider.StreamFn, idle, firstByte time.Duration, label string) provider.StreamFn {
	if inner == nil || idle <= 0 {
		return inner
	}
	return func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		callCtx, cancel := context.WithCancel(ctx)
		in, err := inner(callCtx, model, llm, cfg)
		if err != nil || in == nil {
			cancel()
			return in, err
		}
		out := provider.NewAssistantMessageEventStream(0)
		go relayGuarded(ctx, cancel, in, out, idle, firstByte, callReport{label: label, model: model})
		return out, nil
	}
}

// callReport is what an abnormal end writes to the log. It is counts and
// timings only: no message content, no request headers. See
// spec/turn-lifecycle-and-timeouts.md section 13 — the case that prompted it
// could not be explained from anything that reached disk.
type callReport struct {
	label string // "chat", "compaction", "subagent"
	model string

	events   int           // everything the provider sent
	progress int           // events that actually grew the message
	first    time.Duration // to the first of those; 0 when there was none
	maxGap   time.Duration // the longest run without one
}

// log writes the one line, naming the session and turn when the context has
// them. `progress=0` with a large `events` is the signature of an upstream
// that keeps a connection alive while producing nothing.
func (r callReport) log(ctx context.Context, outcome string, idle, firstByte, elapsed time.Duration) {
	session, turn := "-", "-"
	if info := turnFrom(ctx); info != nil {
		session, turn = shortID(info.sessionID), shortID(info.turnID)
	}
	first := "-"
	if r.first > 0 {
		first = r.first.Round(time.Millisecond).String()
	}
	log.Printf("pigo-server: call %s/%s %s model=%s: %s after %s: events=%d progress=%d first=%s max_gap=%s limits=%s/%s",
		session, turn, r.label, r.model, outcome, elapsed.Round(time.Second),
		r.events, r.progress, first, r.maxGap.Round(time.Millisecond), firstByte, idle)
}

// progressSize measures how much the model has produced so far. A heartbeat or
// an empty delta leaves it unchanged, which is the whole point: the guard
// watches the message growing, not the connection being busy.
func progressSize(msg agentcore.AssistantMessage) int {
	n := 0
	for _, c := range msg.Content {
		switch v := c.(type) {
		case agentcore.TextContent:
			n += len(v.Text)
		case agentcore.ThinkingContent:
			n += len(v.Thinking)
		case agentcore.ToolCallContent:
			n += len(v.Name) + len(v.Arguments)
		}
	}
	return n
}

// relayGuarded forwards the inner stream, restarting the idle timer whenever
// the message grows. When the timer fires it cancels the inner call, emits the
// stall error in its place, and drains what the inner stream still sends so its
// producer can finish.
//
// Until the first output the deadline is the shorter firstByte one; after it,
// the idle one.
func relayGuarded(ctx context.Context, cancel context.CancelFunc, in, out *provider.AssistantMessageEventStream, idle, firstByte time.Duration, report callReport) {
	defer cancel()
	if firstByte <= 0 || firstByte > idle {
		firstByte = idle
	}
	started := time.Now()
	timer := time.NewTimer(firstByte)
	defer timer.Stop()

	var (
		last     agentcore.AssistantMessage
		size     int
		lastAt   = started
		stalled  bool
		finished bool // the provider ended the call itself
		forward  = true
		deadline = firstByte
	)
	events := in.Events()
	for events != nil {
		select {
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			report.events++
			if stalled {
				continue
			}
			if _, ok := ev.(provider.StreamDoneEvent); ok {
				finished = true
			}
			switch e := ev.(type) {
			case provider.StreamStartEvent:
				last = e.Partial
			case provider.StreamTextEvent:
				last = e.Partial
			case provider.StreamThinkingEvent:
				last = e.Partial
			case provider.StreamToolCallEvent:
				last = e.Partial
			}
			if n := progressSize(last); n > size {
				size = n
				now := time.Now()
				if report.progress == 0 {
					report.first = now.Sub(started)
					deadline = idle
				}
				if gap := now.Sub(lastAt); gap > report.maxGap {
					report.maxGap = gap
				}
				lastAt = now
				report.progress++
				timer.Reset(deadline)
			}
			if forward && out.Emit(ctx, ev) != nil {
				forward = false
			}
		case <-timer.C:
			if stalled {
				continue
			}
			stalled = true
			cancel()
			err := &streamStallError{idle: deadline, silent: report.progress == 0}
			msg := last
			msg.RoleField = agentcore.RoleAssistant
			msg.StopReason = agentcore.StopReasonError
			msg.ErrorMessage = err.Error()
			if forward {
				_ = out.Emit(ctx, provider.StreamErrorEvent{Message: msg, Err: err})
			}
		}
	}
	if gap := time.Since(lastAt); gap > report.maxGap {
		report.maxGap = gap
	}
	var result error
	if !stalled {
		if res, err := in.Result(context.Background()); err != nil {
			result = err
			out.SetError(err)
		} else {
			out.SetResult(res)
		}
	}
	// Only an abnormal end is reported: a healthy call is already one ledger
	// row, and a line per call would bury the ones worth reading.
	switch {
	case stalled:
		report.log(ctx, "stalled", idle, firstByte, time.Since(started))
	case ctx.Err() != nil && !finished:
		report.log(ctx, "aborted", idle, firstByte, time.Since(started))
	case result != nil:
		report.log(ctx, "error", idle, firstByte, time.Since(started))
	}
	out.Close()
}

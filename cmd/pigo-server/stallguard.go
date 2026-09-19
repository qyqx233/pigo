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
	"os"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/provider"
)

// defaultStreamIdle matches the transport's own watchdog, and honours the same
// PIGO_STREAM_IDLE_TIMEOUT override so one setting governs both.
const defaultStreamIdle = 5 * time.Minute

func streamIdleTimeout() time.Duration {
	if v := os.Getenv("PIGO_STREAM_IDLE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultStreamIdle
}

// streamStallError reports an abandoned call. It classifies itself as
// transient for provider.IsTransient.
type streamStallError struct{ idle time.Duration }

func (e *streamStallError) Error() string {
	return fmt.Sprintf("the model sent nothing for %s; the call was abandoned", e.idle)
}

func (e *streamStallError) Transient() bool { return true }

// guardStream decorates inner so a call with no event for idle is cancelled
// and ended with a streamStallError.
func guardStream(inner provider.StreamFn, idle time.Duration) provider.StreamFn {
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
		go relayGuarded(ctx, cancel, in, out, idle)
		return out, nil
	}
}

// relayGuarded forwards the inner stream, restarting the idle timer on every
// event. When the timer fires it cancels the inner call, emits the stall error
// in its place, and drains what the inner stream still sends so its producer
// can finish.
func relayGuarded(ctx context.Context, cancel context.CancelFunc, in, out *provider.AssistantMessageEventStream, idle time.Duration) {
	defer cancel()
	timer := time.NewTimer(idle)
	defer timer.Stop()

	var (
		last    agentcore.AssistantMessage
		stalled bool
		forward = true
	)
	events := in.Events()
	for events != nil {
		select {
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if stalled {
				continue
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
			timer.Reset(idle)
			if forward && out.Emit(ctx, ev) != nil {
				forward = false
			}
		case <-timer.C:
			if stalled {
				continue
			}
			stalled = true
			cancel()
			err := &streamStallError{idle: idle}
			msg := last
			msg.RoleField = agentcore.RoleAssistant
			msg.StopReason = agentcore.StopReasonError
			msg.ErrorMessage = err.Error()
			if forward {
				_ = out.Emit(ctx, provider.StreamErrorEvent{Message: msg, Err: err})
			}
		}
	}
	if !stalled {
		if res, err := in.Result(context.Background()); err != nil {
			out.SetError(err)
		} else {
			out.SetResult(res)
		}
	}
	out.Close()
}

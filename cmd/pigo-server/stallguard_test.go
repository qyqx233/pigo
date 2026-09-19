package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/provider"
)

// hangingStream starts a call, sends one event, and then says nothing until
// its context is cancelled — a hung upstream. It reports when it saw the
// cancellation.
func hangingStream(first provider.AssistantMessageEvent, cancelled chan<- struct{}) provider.StreamFn {
	return func(ctx context.Context, _ string, _ provider.LlmContext, _ provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		s := provider.NewAssistantMessageEventStream(0)
		go func() {
			defer s.Close()
			if first != nil && s.Emit(ctx, first) != nil {
				return
			}
			<-ctx.Done()
			close(cancelled)
			msg := agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonAborted}
			_ = s.Emit(context.Background(), provider.StreamErrorEvent{Message: msg, Err: ctx.Err()})
		}()
		return s, nil
	}
}

func collect(t *testing.T, s *provider.AssistantMessageEventStream) []provider.AssistantMessageEvent {
	t.Helper()
	var out []provider.AssistantMessageEvent
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatal("stream did not end")
		}
	}
}

// TestStallGuardAbandonsSilentCall: a call that sends nothing is cancelled and
// ends with an error the retry policy treats as transient.
func TestStallGuardAbandonsSilentCall(t *testing.T) {
	cancelled := make(chan struct{})
	guarded := guardStream(hangingStream(nil, cancelled), 50*time.Millisecond)
	s, err := guarded(context.Background(), "m", provider.LlmContext{}, provider.StreamConfig{})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, s)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("the upstream call was not cancelled")
	}
	if len(events) != 1 {
		t.Fatalf("events = %#v, want exactly the stall error (the upstream's own abort must not leak)", events)
	}
	failure, ok := events[0].(provider.StreamErrorEvent)
	if !ok {
		t.Fatalf("event = %#v", events[0])
	}
	var stall *streamStallError
	if !errors.As(failure.Err, &stall) || !provider.IsTransient(failure.Err) {
		t.Errorf("err = %v; want a transient stall error", failure.Err)
	}
	if failure.Message.StopReason != agentcore.StopReasonError {
		t.Errorf("stop reason = %q", failure.Message.StopReason)
	}
	if res, err := s.Result(context.Background()); err != nil || res.StopReason != agentcore.StopReasonError {
		t.Errorf("result = %+v, %v", res, err)
	}
}

// TestStallGuardKeepsPartial: a call that stalls mid-answer keeps what it had
// produced on the failed message.
func TestStallGuardKeepsPartial(t *testing.T) {
	partial := agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant,
		Content: agentcore.ContentList{agentcore.NewTextContent("half an ans")}}
	cancelled := make(chan struct{})
	guarded := guardStream(hangingStream(provider.StreamTextEvent{Partial: partial}, cancelled), 50*time.Millisecond)
	s, _ := guarded(context.Background(), "m", provider.LlmContext{}, provider.StreamConfig{})
	events := collect(t, s)
	if len(events) != 2 {
		t.Fatalf("events = %d", len(events))
	}
	failure := events[1].(provider.StreamErrorEvent)
	if agentcore.ContentToText(failure.Message.Content) != "half an ans" {
		t.Errorf("partial lost: %+v", failure.Message)
	}
}

// TestStallGuardLeavesProgressingCall: a slow call that keeps sending is never
// cut, however long it takes in total.
func TestStallGuardLeavesProgressingCall(t *testing.T) {
	slow := func(ctx context.Context, _ string, _ provider.LlmContext, _ provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		s := provider.NewAssistantMessageEventStream(0)
		go func() {
			defer s.Close()
			msg := agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}
			for i := 0; i < 10; i++ {
				time.Sleep(20 * time.Millisecond)
				if s.Emit(ctx, provider.StreamTextEvent{Partial: msg}) != nil {
					return
				}
			}
			msg.StopReason = agentcore.StopReasonEndTurn
			_ = s.Emit(ctx, provider.StreamDoneEvent{Message: msg})
		}()
		return s, nil
	}
	// 200ms in total, never more than 20ms between events, guard at 60ms.
	s, _ := guardStream(slow, 60*time.Millisecond)(context.Background(), "m", provider.LlmContext{}, provider.StreamConfig{})
	events := collect(t, s)
	if _, ok := events[len(events)-1].(provider.StreamDoneEvent); !ok || len(events) != 11 {
		t.Fatalf("events = %d, last = %#v", len(events), events[len(events)-1])
	}
}

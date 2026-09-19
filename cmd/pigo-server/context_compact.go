// Context compaction in the web server: the automatic kind runs inside the
// agent loop once a turn's context passes its threshold (the parameters are
// set per turn, context_params.go); /compact runs it on request, as a turn of
// its own. Both report the same "compaction" events, which the activity log
// shows, and both leave the transcript's history in place (transcript_file.go).
//
// See spec/context-compaction.md.
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/compaction"
	"github.com/smallnest/pigo/internal/provider"
)

// compactionReport is a compaction's figures, in estimated context tokens.
type compactionReport struct {
	// Reason is "threshold" (automatic) or "manual" (/compact).
	Reason     string `json:"reason,omitempty"`
	Before     int    `json:"before,omitempty"`
	After      int    `json:"after,omitempty"`
	Summarized int    `json:"summarized,omitempty"`
}

// compactionEnd is the "compaction" end event for an agent CompactionEvent.
func compactionEnd(e agentcore.CompactionEvent) streamEvent {
	return streamEvent{
		Type:       "compaction",
		Phase:      "end",
		IsError:    e.ErrorMessage != "",
		Error:      e.ErrorMessage,
		Compaction: &compactionReport{Reason: e.Reason, Before: e.TokensBefore, After: e.TokensAfter, Summarized: e.SummarizedCount},
	}
}

// skippedCompaction reports an automatic compaction that found nothing old
// enough to summarize; it leaves no trace in the activity log.
func skippedCompaction(ev streamEvent) bool {
	c := ev.Compaction
	return !ev.IsError && c != nil && c.Reason != "manual" && c.Summarized == 0
}

// isCompactCommand reports whether a prompt is /compact. It takes no
// arguments; anything after it is ignored rather than sent to the model.
func isCompactCommand(prompt string) bool {
	fields := strings.Fields(prompt)
	return len(fields) > 0 && fields[0] == "/compact"
}

// compactSession is /compact's turn: it summarizes the session's context now,
// whatever its size against the threshold, keeping the recent part.
func (s *apiServer) compactSession(ctx context.Context, managed *managedSession, run *turnRun) (string, error) {
	managed.mu.Lock()
	if err := s.ensureHostLoop(managed); err != nil {
		managed.mu.Unlock()
		return "", err
	}
	if err := s.errNoProviderKey(managed.meta.UserID, managed.meta.Provider); err != nil {
		managed.mu.Unlock()
		return "", err
	}
	// The turn holds the session, so no other turn changes the context
	// while it is summarized.
	msgs := append(agentcore.MessageList(nil), managed.agentCtx.Messages...)
	cfg := managed.runCfg
	params := s.contextParams(managed.meta.Provider, managed.meta.Model)
	turn := s.newTurn(managed, run.publish)
	managed.mu.Unlock()
	turn.hold()
	ctx = withTurn(ctx, turn)
	defer func() {
		turn.wait(10 * time.Second)
		run.addLedger(turn.release())
	}()

	before := compaction.EstimateContextTokens(msgs).Tokens
	run.publish(streamEvent{Type: "compaction", Phase: "start", Compaction: &compactionReport{Reason: "manual", Before: before}})
	key := ""
	if cfg.GetAPIKey != nil {
		key = cfg.GetAPIKey(ctx, cfg.Provider)
	}
	model := provider.Model{Provider: cfg.Provider, ID: cfg.Model, ContextWindow: params.Window}
	res, err := compaction.Compact(ctx, cfg.SummaryStream, model, msgs, params.settings(), -1, nil, "",
		provider.StreamConfig{APIKey: key, ThinkingLevel: cfg.ThinkingLevel})
	if err != nil {
		run.publish(compactionEnd(agentcore.CompactionEvent{Reason: "manual", TokensBefore: before, TokensAfter: before, ErrorMessage: err.Error()}))
		return "", fmt.Errorf("压缩失败：%v", err)
	}
	if res == nil {
		run.publish(compactionEnd(agentcore.CompactionEvent{Reason: "manual", TokensBefore: before, TokensAfter: before}))
		return "上下文还很短，没有可以压缩的内容。", nil
	}

	rebuilt := res.RebuildContext(msgs, time.Now().UnixMilli())
	managed.mu.Lock()
	managed.agentCtx.Messages = rebuilt
	if s.checkpointLocked(managed, rebuilt) {
		run.transcriptRewritten()
	}
	managed.mu.Unlock()

	after := compaction.EstimateContextTokens(rebuilt).Tokens
	summarized := len(msgs) - (len(rebuilt) - 1)
	run.publish(compactionEnd(agentcore.CompactionEvent{Reason: "manual", TokensBefore: before, TokensAfter: after, SummarizedCount: summarized, KeptCount: len(rebuilt) - 1}))
	return fmt.Sprintf("已压缩上下文（原约 %s token）：总结了 %d 条较早的消息，保留最近 %d 条。下一次回复的用量行会显示压缩后的大小。",
		formatTokenCount(before), summarized, len(rebuilt)-1), nil
}

// formatTokenCount renders a token count briefly: 950, 12.3k, 1.2M.
func formatTokenCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/session"
)

// historyScene names the scene of a scene command.
type historyScene struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type historyMessage struct {
	ID         string    `json:"id"`
	Role       string    `json:"role"`
	Content    string    `json:"content,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	ToolName   string    `json:"toolName,omitempty"`
	ToolCallID string    `json:"toolCallId,omitempty"`
	Arguments  any       `json:"arguments,omitempty"`
	IsError    bool      `json:"isError,omitempty"`
	// Detail (a tool call's one-line description) and Summary (a failed
	// result's one line) are what the activity log shows (activity.go).
	Detail  string `json:"detail,omitempty"`
	Summary string `json:"summary,omitempty"`
	// Usage is the turn's model usage and cost, on the turn's last assistant
	// message (see attachTurnUsage).
	Usage *turnUsage `json:"usage,omitempty"`
	// Compaction is a "compaction" message's figures; its Content is the
	// summary that replaced the earlier conversation.
	Compaction *compactionReport `json:"compaction,omitempty"`
	// Scene is set on a user message sent as a scene command; Content is then
	// the command as typed ("/slug question"), not the expanded prompt.
	Scene *historyScene `json:"scene,omitempty"`

	// turn numbers the user turns: it goes up with each user message.
	turn int
}

func (s *apiServer) handleListSessions(w http.ResponseWriter, r *http.Request) {
	principal := principalForRequest(r)
	s.mu.RLock()
	all := make([]*managedSession, 0, len(s.sessions))
	for _, managed := range s.sessions {
		all = append(all, managed)
	}
	s.mu.RUnlock()

	result := make([]map[string]any, 0, len(all))
	for _, managed := range all {
		managed.mu.Lock()
		// A draft is not listed: nothing has been said in it yet.
		if !managed.closed && !managed.meta.draft && principalCanAccess(principal, managed.meta) {
			if managed.meta.Title == "" {
				managed.meta.Title = s.deriveSessionTitle(managed)
				if managed.meta.Title != "" {
					_ = s.saveSession(managed.meta)
				}
			}
			result = append(result, s.sessionJSON(managed))
		}
		managed.mu.Unlock()
	}
	sort.Slice(result, func(i, j int) bool {
		left, _ := result[i]["lastUsed"].(time.Time)
		right, _ := result[j]["lastUsed"].(time.Time)
		return left.After(right)
	})
	writeJSON(w, http.StatusOK, result)
}

func (s *apiServer) handleSessionMessages(w http.ResponseWriter, r *http.Request) {
	managed, ok := s.sessionForRequest(r, r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	managed.mu.Lock()
	defer managed.mu.Unlock()
	if managed.closed {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	entries := s.loadTranscriptEntries(managed)
	// While a turn runs, what it has produced after the user's message is
	// served by the turn's snapshot (GET .../turn); showing it here as well
	// would show it twice.
	if run := managed.activeTurn(); run != nil {
		if cut := run.historyCut(); cut >= 0 && cut < len(entries) {
			entries = entries[:cut]
		}
	}
	messages := historyMessages(managed.meta.ID, entries, func(name string, args json.RawMessage) string {
		return s.toolDetail(nil, name, args)
	})
	attachTurnUsage(messages, responseTurns(entries), s.sessionLedger(managed.meta))
	writeJSON(w, http.StatusOK, messages)
}

func (s *apiServer) sessionForRequest(r *http.Request, id string) (*managedSession, bool) {
	managed, ok := s.getSession(id)
	if !ok {
		return nil, false
	}
	managed.mu.Lock()
	allowed := !managed.closed && principalCanAccess(principalForRequest(r), managed.meta)
	managed.mu.Unlock()
	return managed, allowed
}

func principalCanAccess(principal requestPrincipal, meta sessionMeta) bool {
	if principal.Service {
		return true
	}
	return principal.UserID != "" && meta.UserID == principal.UserID
}

func (s *apiServer) claimLegacySessions(userID string) {
	s.mu.RLock()
	all := make([]*managedSession, 0, len(s.sessions))
	for _, managed := range s.sessions {
		all = append(all, managed)
	}
	s.mu.RUnlock()
	for _, managed := range all {
		managed.mu.Lock()
		if managed.meta.UserID == "" && !managed.closed {
			managed.meta.UserID = userID
			if managed.meta.Title == "" {
				managed.meta.Title = s.deriveSessionTitle(managed)
			}
			_ = s.saveSession(managed.meta)
		}
		managed.mu.Unlock()
	}
}

func (s *apiServer) deriveSessionTitle(managed *managedSession) string {
	for _, entry := range s.loadTranscriptEntries(managed) {
		if message, ok := entry.Message.(agentcore.UserMessage); ok {
			if title := cleanSessionTitle(agentcore.ContentToText(message.Content)); title != "" {
				return title
			}
		}
	}
	return ""
}

func cleanSessionTitle(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	const maxRunes = 48
	if utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	runes := []rune(value)
	return string(runes[:maxRunes]) + "…"
}

// detail says what a tool call shows in the activity log (apiServer.toolDetail).
func historyMessages(sessionID string, entries []session.Entry, detail func(name string, args json.RawMessage) string) []historyMessage {
	result := make([]historyMessage, 0, len(entries))
	turn := 0
	for index, entry := range entries {
		baseID := fmt.Sprintf("%s-%d", sessionID, index)
		first := len(result)
		switch message := entry.Message.(type) {
		case agentcore.UserMessage:
			turn++
			if text := agentcore.ContentToText(message.Content); text != "" {
				m := historyMessage{ID: baseID, Role: agentcore.RoleUser, Content: text, CreatedAt: entry.Timestamp}
				if slug, name, question, ok := parseSceneCommand(text); ok {
					m.Content = "/" + slug + " " + question
					m.Scene = &historyScene{Slug: slug, Name: name}
				}
				result = append(result, m)
			}
		case agentcore.AssistantMessage:
			part := 0
			for _, content := range message.Content {
				switch item := content.(type) {
				case agentcore.TextContent:
					if item.Text != "" {
						result = append(result, historyMessage{ID: fmt.Sprintf("%s-%d", baseID, part), Role: agentcore.RoleAssistant, Content: item.Text, CreatedAt: entry.Timestamp})
						part++
					}
				case agentcore.ToolCallContent:
					result = append(result, historyMessage{ID: fmt.Sprintf("%s-%d", baseID, part), Role: "toolCall", CreatedAt: entry.Timestamp, ToolName: item.Name, ToolCallID: item.ID, Arguments: historyArguments(item.Arguments), Detail: detail(item.Name, item.Arguments)})
					part++
				}
			}
		case agentcore.CompactionMessage:
			meta := readCompactionMeta(message.Details)
			result = append(result, historyMessage{ID: baseID, Role: agentcore.RoleCompaction, Content: message.Summary, CreatedAt: entry.Timestamp,
				Compaction: &compactionReport{Before: message.TokensBefore, After: meta.TokensAfter, Summarized: meta.Summarized}})
		case agentcore.ToolResultMessage:
			output := agentcore.ContentToText(message.Content)
			result = append(result, historyMessage{ID: baseID, Role: agentcore.RoleToolResult, Content: truncateMiddle(output, historyOutputLimit), CreatedAt: entry.Timestamp, ToolName: message.ToolName, ToolCallID: message.ToolCallID, IsError: message.IsError, Summary: toolFailure(output, message.IsError)})
		}
		for i := first; i < len(result); i++ {
			result[i].turn = turn
		}
	}
	return result
}

// responseTurns maps each model response in a transcript to its user turn,
// numbered as historyMessages numbers them.
func responseTurns(entries []session.Entry) map[string]int {
	out := map[string]int{}
	turn := 0
	for _, entry := range entries {
		switch message := entry.Message.(type) {
		case agentcore.UserMessage:
			turn++
		case agentcore.AssistantMessage:
			if message.ResponseID != "" {
				out[message.ResponseID] = turn
			}
		}
	}
	return out
}

// historyOutputLimit bounds a tool's output in history; the head and tail are
// kept.
const historyOutputLimit = 4000

func historyArguments(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	return string(raw)
}

func workspaceIsEmpty(paths sessionPaths) bool {
	entries, err := os.ReadDir(paths.Workspace)
	// A draft has no directory at all until its first message. Reading that as
	// "not empty" would keep every abandoned draft in memory for the life of
	// the process, since the reaper skips a session whose workspace has files.
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	return err == nil && len(entries) == 0
}

func (s *apiServer) removeExpiredEmptySession(id string, managed *managedSession, now time.Time) {
	s.mu.Lock()
	current, ok := s.sessions[id]
	if !ok || current != managed || !managed.mu.TryLock() {
		s.mu.Unlock()
		return
	}
	if managed.closed || managed.activeTurn() != nil || managed.liveAlive() || now.Sub(managed.meta.LastUsed) < s.config.emptySessionTTL || hasTranscript(managed.paths) || !workspaceIsEmpty(managed.paths) {
		managed.mu.Unlock()
		s.mu.Unlock()
		return
	}
	delete(s.sessions, id)
	managed.closed = true
	paths := managed.paths
	managed.mu.Unlock()
	s.mu.Unlock()
	if err := s.removeSession(id, paths); err != nil {
		// Cleanup is best effort. The directory will be discovered again only on
		// restart, where it remains eligible for the next cleanup pass.
		return
	}
}

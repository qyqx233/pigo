package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/session"
)

type historyMessage struct {
	ID         string    `json:"id"`
	Role       string    `json:"role"`
	Content    string    `json:"content,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	ToolName   string    `json:"toolName,omitempty"`
	ToolCallID string    `json:"toolCallId,omitempty"`
	Arguments  any       `json:"arguments,omitempty"`
	IsError    bool      `json:"isError,omitempty"`
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
		if !managed.closed && principalCanAccess(principal, managed.meta) {
			if managed.meta.Title == "" {
				managed.meta.Title = s.deriveSessionTitle(managed)
				if managed.meta.Title != "" {
					_ = managed.paths.saveMeta(managed.meta)
				}
			}
			result = append(result, sessionResponse(managed))
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
	writeJSON(w, http.StatusOK, historyMessages(managed.meta.ID, entries))
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
			_ = managed.paths.saveMeta(managed.meta)
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

func (s *apiServer) loadTranscriptEntries(managed *managedSession) []session.Entry {
	store, err := session.NewStore(filepath.Join(managed.paths.Root, "transcript"))
	if err != nil {
		return nil
	}
	_, entries, err := store.LoadEntries(transcriptID)
	if err != nil {
		return nil
	}
	return entries
}

func historyMessages(sessionID string, entries []session.Entry) []historyMessage {
	result := make([]historyMessage, 0, len(entries))
	for index, entry := range entries {
		baseID := fmt.Sprintf("%s-%d", sessionID, index)
		switch message := entry.Message.(type) {
		case agentcore.UserMessage:
			if text := agentcore.ContentToText(message.Content); text != "" {
				result = append(result, historyMessage{ID: baseID, Role: agentcore.RoleUser, Content: text, CreatedAt: entry.Timestamp})
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
					result = append(result, historyMessage{ID: fmt.Sprintf("%s-%d", baseID, part), Role: "toolCall", CreatedAt: entry.Timestamp, ToolName: item.Name, ToolCallID: item.ID, Arguments: historyArguments(item.Arguments)})
					part++
				}
			}
		case agentcore.ToolResultMessage:
			result = append(result, historyMessage{ID: baseID, Role: agentcore.RoleToolResult, Content: agentcore.ContentToText(message.Content), CreatedAt: entry.Timestamp, ToolName: message.ToolName, ToolCallID: message.ToolCallID, IsError: message.IsError})
		}
	}
	return result
}

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

func sessionHasNoTranscript(paths sessionPaths) bool {
	entries, err := os.ReadDir(filepath.Join(paths.Root, "transcript"))
	return os.IsNotExist(err) || (err == nil && len(entries) == 0)
}

func workspaceIsEmpty(paths sessionPaths) bool {
	entries, err := os.ReadDir(paths.Workspace)
	return err == nil && len(entries) == 0
}

func (s *apiServer) removeExpiredEmptySession(id string, managed *managedSession, now time.Time) {
	s.mu.Lock()
	current, ok := s.sessions[id]
	if !ok || current != managed || !managed.mu.TryLock() {
		s.mu.Unlock()
		return
	}
	if managed.closed || managed.busy || managed.liveAlive() || now.Sub(managed.meta.LastUsed) < s.config.emptySessionTTL || !sessionHasNoTranscript(managed.paths) || !workspaceIsEmpty(managed.paths) {
		managed.mu.Unlock()
		s.mu.Unlock()
		return
	}
	delete(s.sessions, id)
	managed.closed = true
	paths := managed.paths
	managed.mu.Unlock()
	s.mu.Unlock()
	if err := paths.remove(); err != nil {
		// Cleanup is best effort. The directory will be discovered again only on
		// restart, where it remains eligible for the next cleanup pass.
		return
	}
}

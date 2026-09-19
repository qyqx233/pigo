package agentcore

import (
	"encoding/json"
	"fmt"
)

// Message roles, matching pi's wire format.
const (
	RoleUser       = "user"
	RoleAssistant  = "assistant"
	RoleToolResult = "toolResult"
	// RoleCompaction marks a compaction checkpoint persisted inline in the
	// message list: it replaces the history summarized before it (pi's
	// "compactionSummary"). It is not sent to the model verbatim; the LLM
	// conversion turns it into a user text block.
	RoleCompaction = "compaction"
)

// Message is the sealed interface implemented by the three message roles.
// AgentMessage (the loop's message abstraction) is simply Message: custom
// message kinds implement the same interface and convertToLlm filters out any
// that are not LLM-bound. This deliberately replaces pi's declaration merging,
// which has no Go equivalent.
type Message interface {
	isMessage()
	// Role returns the discriminant ("user" | "assistant" | "toolResult").
	Role() string
}

// AgentMessage is the loop-level message type. It is the same as Message; the
// alias documents intent at call sites that deal with the loop rather than raw
// LLM messages.
type AgentMessage = Message

// Usage reports token accounting for an assistant response, in pi's shape: the
// four token counts never overlap, so their sum is the whole request plus
// response and each can be priced on its own.
//
// Providers disagree about what "input" means — OpenAI-style APIs report a total
// that already includes cache hits, Anthropic reports input without them — so
// every decoder converts its own wire format into this one before anything
// downstream sees it. InputTokens is therefore always the input that was NOT
// served from cache.
type Usage struct {
	// InputTokens is input that missed the prompt cache.
	InputTokens int `json:"inputTokens"`
	// OutputTokens is everything generated, reasoning included.
	OutputTokens int `json:"outputTokens"`
	// CacheReadTokens is input served from the provider's prompt cache.
	CacheReadTokens int `json:"cacheReadTokens,omitempty"`
	// CacheWriteTokens is input written into the prompt cache on this request
	// (reported by Anthropic-style APIs; priced above plain input there).
	CacheWriteTokens int `json:"cacheWriteTokens,omitempty"`
	// ReasoningTokens is the part of OutputTokens spent on reasoning, for
	// display only: it is already counted in OutputTokens.
	ReasoningTokens int `json:"reasoningTokens,omitempty"`
	// UpstreamCostUSD is what the provider says the call cost, when it says so
	// (OpenRouter does on request). Nil means not reported.
	UpstreamCostUSD *float64 `json:"upstreamCostUsd,omitempty"`
	// Anomaly marks a response whose numbers did not add up (a cache count
	// larger than the input total) and were therefore taken without the usual
	// subtraction. It lets such calls be found afterwards instead of silently
	// trusted.
	Anomaly bool `json:"anomaly,omitempty"`
}

// TotalInputTokens is the whole prompt as the model saw it, cached or not.
func (u Usage) TotalInputTokens() int {
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// ContextTokens is the size of the conversation after this response: the full
// prompt plus what was generated. Compaction uses it to decide when the
// context is getting full, which is why cache hits must be in it.
func (u Usage) ContextTokens() int {
	return u.TotalInputTokens() + u.OutputTokens
}

// IsZero reports whether the response carried no token accounting at all.
func (u Usage) IsZero() bool {
	return u.TotalInputTokens() == 0 && u.OutputTokens == 0
}

// UserMessage is input from the user. Content is restricted at construction to
// text/image blocks (runtime constraint, not a separate interface).
type UserMessage struct {
	RoleField string      `json:"role"`
	Content   ContentList `json:"content"`
	Timestamp int64       `json:"timestamp"`
}

func (UserMessage) isMessage()     {}
func (m UserMessage) Role() string { return RoleUser }

// AssistantMessage is a model response. Content may hold text/thinking/toolCall
// blocks. StopReason follows pi's set (end_turn/tool_use/length/error/aborted).
type AssistantMessage struct {
	RoleField    string      `json:"role"`
	Content      ContentList `json:"content"`
	API          string      `json:"api,omitempty"`
	Provider     string      `json:"provider,omitempty"`
	Model        string      `json:"model,omitempty"`
	Usage        *Usage      `json:"usage,omitempty"`
	StopReason   string      `json:"stopReason,omitempty"`
	ErrorMessage string      `json:"errorMessage,omitempty"`
	Timestamp    int64       `json:"timestamp"`

	// Optional diagnostics, kept for cross-provider replay/observability.
	ResponseModel string `json:"responseModel,omitempty"`
	ResponseID    string `json:"responseId,omitempty"`
}

func (AssistantMessage) isMessage()     {}
func (m AssistantMessage) Role() string { return RoleAssistant }

// ToolCalls returns the tool call blocks in this assistant message, in order.
func (m AssistantMessage) ToolCalls() []ToolCallContent {
	var calls []ToolCallContent
	for _, c := range m.Content {
		if tc, ok := c.(ToolCallContent); ok {
			calls = append(calls, tc)
		}
	}
	return calls
}

// ToolResultMessage carries the outcome of executing a single tool call.
// Content is restricted to text/image blocks at construction.
type ToolResultMessage struct {
	RoleField  string      `json:"role"`
	ToolCallID string      `json:"toolCallId"`
	ToolName   string      `json:"toolName"`
	Content    ContentList `json:"content"`
	Details    any         `json:"details,omitempty"`
	IsError    bool        `json:"isError"`
	Timestamp  int64       `json:"timestamp"`
}

func (ToolResultMessage) isMessage()     {}
func (m ToolResultMessage) Role() string { return RoleToolResult }

// CompactionMessage is a summarization checkpoint persisted inline in the
// message list. It stands in for the history compacted before it: Summary is
// the structured checkpoint text and TokensBefore records the estimated context
// size at compaction time (for observability). Details optionally holds the
// file operations extracted from the compacted range. Mirrors pi's
// CompactionSummaryMessage + CompactionEntry.
type CompactionMessage struct {
	RoleField    string `json:"role"`
	Summary      string `json:"summary"`
	TokensBefore int    `json:"tokensBefore,omitempty"`
	// Details is opaque at this layer (the compaction package owns its shape);
	// kept as raw JSON so agentcore stays free of a compaction dependency.
	Details   json.RawMessage `json:"details,omitempty"`
	Timestamp int64           `json:"timestamp"`
}

func (CompactionMessage) isMessage()     {}
func (m CompactionMessage) Role() string { return RoleCompaction }

// compactionSummaryPrefix / compactionSummarySuffix wrap a compaction summary
// when it is rendered into an LLM user message, matching pi's
// COMPACTION_SUMMARY_PREFIX / COMPACTION_SUMMARY_SUFFIX.
const (
	compactionSummaryPrefix = "The conversation history before this point was compacted into the following summary:\n\n<summary>\n"
	compactionSummarySuffix = "\n</summary>"
)

// AsUserMessage renders a compaction checkpoint as the user text message that
// stands in for the compacted history when building the LLM request. The
// provider encoders call this so a persisted compaction line replays as
// context rather than being dropped.
func (m CompactionMessage) AsUserMessage() UserMessage {
	return UserMessage{
		RoleField: RoleUser,
		Content:   ContentList{NewTextContent(compactionSummaryPrefix + m.Summary + compactionSummarySuffix)},
		Timestamp: m.Timestamp,
	}
}

// StopReason values, matching pi.
const (
	StopReasonEndTurn = "end_turn"
	StopReasonToolUse = "tool_use"
	StopReasonLength  = "length"
	StopReasonError   = "error"
	StopReasonAborted = "aborted"
)

// MessageList is a slice of Message with discriminated JSON (un)marshalling,
// dispatching on the "role" field. Used by AgentContext and session persistence.
type MessageList []Message

// UnmarshalJSON decodes a JSON array of messages, dispatching each element on
// its "role" discriminant.
func (ml *MessageList) UnmarshalJSON(data []byte) error {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return err
	}
	out := make(MessageList, 0, len(raws))
	for i, raw := range raws {
		m, err := decodeMessage(raw)
		if err != nil {
			return fmt.Errorf("message[%d]: %w", i, err)
		}
		out = append(out, m)
	}
	*ml = out
	return nil
}

// decodeMessage peeks at the "role" field and decodes into the matching
// concrete message struct.
func decodeMessage(raw json.RawMessage) (Message, error) {
	var probe struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("peek role: %w", err)
	}
	switch probe.Role {
	case RoleUser:
		var m UserMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return m, nil
	case RoleAssistant:
		var m AssistantMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return m, nil
	case RoleToolResult:
		var m ToolResultMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return m, nil
	case RoleCompaction:
		var m CompactionMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return m, nil
	case "":
		return nil, fmt.Errorf("missing role discriminant")
	default:
		return nil, fmt.Errorf("unknown role %q", probe.Role)
	}
}

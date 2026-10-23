package claudeprint

import "encoding/json"

// streamEvent is the union of every `claude -p --output-format stream-json
// --verbose` line claudeprint interprets. Fields it does not recognize are
// left in place by encoding/json (ignored, not an error), so a claude
// release that adds new event shapes never breaks parsing of the ones
// documented here.
type streamEvent struct {
	Type    string         `json:"type"`
	Subtype string         `json:"subtype,omitempty"`
	Message *streamMessage `json:"message,omitempty"`

	// type: "result" fields.
	Result       string  `json:"result,omitempty"`
	IsError      bool    `json:"is_error,omitempty"`
	SessionID    string  `json:"session_id,omitempty"`
	NumTurns     int     `json:"num_turns,omitempty"`
	TotalCostUSD float64 `json:"total_cost_usd,omitempty"`
	DurationMS   int64   `json:"duration_ms,omitempty"`
}

// streamMessage is the "message" field of an "assistant" (or "user")
// stream-json event: an Anthropic Messages-API-shaped message with a list
// of content blocks.
type streamMessage struct {
	Role    string               `json:"role,omitempty"`
	Content []streamContentBlock `json:"content,omitempty"`
}

// streamContentBlock is one content block of an assistant message: either
// "text" (Text set) or "tool_use" (ID/Name/Input set).
type streamContentBlock struct {
	Type  string         `json:"type"`
	Text  string         `json:"text,omitempty"`
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input map[string]any `json:"input,omitempty"`
}

// parseStreamEvent parses one stream-json line. It returns ok=false (not an
// error) for a line that isn't a recognizable JSON object — stream-json
// output is line-delimited, so a stray blank line or partial write should
// not abort the whole turn.
func parseStreamEvent(line []byte) (streamEvent, bool) {
	var ev streamEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return streamEvent{}, false
	}
	if ev.Type == "" {
		return streamEvent{}, false
	}
	return ev, true
}

// toolCallsFrom extracts every tool_use content block of an assistant
// message as a ToolCall.
func toolCallsFrom(msg *streamMessage) []ToolCall {
	if msg == nil {
		return nil
	}
	var calls []ToolCall
	for _, block := range msg.Content {
		if block.Type != "tool_use" {
			continue
		}
		call := ToolCall{Name: block.Name, Input: block.Input}
		if cmd, ok := block.Input["command"].(string); ok {
			call.Command = cmd
		}
		calls = append(calls, call)
	}
	return calls
}

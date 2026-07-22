package anthropic_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/chatwright/chatwright/actor"
	"github.com/chatwright/chatwright/actor/anthropic"
	"github.com/chatwright/chatwright/observe"
)

// roundTripFunc adapts a function to http.RoundTripper, the same way
// http.HandlerFunc adapts a function to http.Handler — every test in this
// package drives a Provider through one of these instead of the network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// countingTransport wraps handle and records how many requests reached it —
// the load-bearing assertion for the cassette-composition test (replay mode
// must make zero).
type countingTransport struct {
	handle func(*http.Request) (*http.Response, error)

	mu    sync.Mutex
	calls int
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.handle(r)
}

func (c *countingTransport) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// jsonResponse builds an *http.Response with status and a JSON-encoded body.
func jsonResponse(t *testing.T, status int, body any) *http.Response {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal fake response body: %v", err)
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(data)),
	}
}

// messagesAPISuccess builds a minimal, valid Anthropic Messages API success
// response body carrying text as its sole content block.
func messagesAPISuccess(model, text string, inputTokens, outputTokens int64) map[string]any {
	return map[string]any{
		"id":    "msg_test",
		"type":  "message",
		"role":  "assistant",
		"model": model,
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	}
}

// messagesAPIRefusal builds a response with stop_reason "refusal" and no
// content — the shape a safety-classifier decline takes per the Claude API.
func messagesAPIRefusal(model string) map[string]any {
	return map[string]any{
		"id":            "msg_test",
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       []map[string]any{},
		"stop_reason":   "refusal",
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  0,
			"output_tokens": 0,
		},
	}
}

// messagesAPIError builds an Anthropic-shaped error envelope body.
func messagesAPIError(errType, message string) map[string]any {
	return map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": message,
		},
	}
}

// decodeRequestBody reads and JSON-decodes r's body.
func decodeRequestBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("decode request body: %v (%s)", err, data)
	}
	return body
}

// singleReply returns a transport that always replies 200 OK with reply as
// the sole text content block, using DefaultModel and fixed token counts.
func singleReply(reply string) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     http.StatusText(http.StatusOK),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewReader(mustJSON(messagesAPISuccess(
				anthropic.DefaultModel, reply, 42, 7,
			)))),
		}, nil
	})
}

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

// stepClock returns a clock that advances by step on every call, starting
// at a fixed epoch — deterministic and non-zero, so Usage.Latency
// assertions do not depend on wall-clock timing.
func stepClock(step time.Duration) func() time.Time {
	cur := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	return func() time.Time {
		now := cur
		cur = cur.Add(step)
		return now
	}
}

// newTestProvider builds a Provider wired to transport, a fixed test API
// key and zero SDK retries (so error-taxonomy tests assert on the first
// response instead of waiting through real backoff delays).
func newTestProvider(t *testing.T, transport http.RoundTripper, opts ...func(*anthropic.Config)) *anthropic.Provider {
	t.Helper()
	zero := 0
	cfg := anthropic.Config{
		APIKey:     "test-key",
		HTTPClient: &http.Client{Transport: transport},
		Now:        stepClock(10 * time.Millisecond),
		MaxRetries: &zero,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	p, err := anthropic.New(cfg)
	if err != nil {
		t.Fatalf("anthropic.New: %v", err)
	}
	return p
}

// usagesEqual compares two actor.Usage values by content: actor.Usage.Cost
// is a *float64, so a plain struct == would compare pointer identity, which
// differs after a JSON round-trip (cassette save/load, in particular) even
// when the pointed-to value is identical.
func usagesEqual(a, b actor.Usage) bool {
	if a.Model != b.Model || a.InputTokens != b.InputTokens || a.OutputTokens != b.OutputTokens || a.Latency != b.Latency {
		return false
	}
	switch {
	case a.Cost == nil && b.Cost == nil:
		return true
	case a.Cost == nil || b.Cost == nil:
		return false
	default:
		return *a.Cost == *b.Cost
	}
}

// samplePrompt is a representative actor.Prompt shared across tests: a
// goal/task pair, one bot message with one available action, and no history
// yet.
func samplePrompt() actor.Prompt {
	return actor.Prompt{
		GoalID:          "listus-shopping-list",
		GoalTitle:       "Add items to the shopping list",
		GoalDescription: "Verify a user can add multiple items in one message.",
		Constraints:     []string{"Do not use admin-only commands."},

		TaskID:              "add-items",
		TaskTitle:           "Add milk, eggs, bread",
		TaskSuccessCriteria: "The bot confirms the three items were added.",

		Observation: observe.Observation{
			Sequence: 2,
			Chat:     observe.ChatRef{ChatID: 42},
			Messages: []observe.VisibleMessage{
				{
					ID:    "msg7",
					Actor: observe.ActorBot,
					Text:  "What would you like to add?",
					Actions: []observe.AvailableAction{
						{ID: "btn-cancel", Label: "Cancel", SeenAt: 2},
					},
				},
			},
		},
	}
}

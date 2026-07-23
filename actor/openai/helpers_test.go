package openai_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/actor/openai"
	"chatwright.dev/runtime/observe"
)

// roundTripFunc adapts a function to http.RoundTripper, the same way
// http.HandlerFunc adapts a function to http.Handler — used only for the
// handful of tests that need a transport-level failure (connection
// refused, cancelled context) a real listener cannot simulate reliably;
// every other test in this package drives a Provider against a real
// httptest.Server, matching the task's "httptest fake OpenAI-compatible
// server" requirement.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// loggedRequest is one request a fakeServer observed, retained for
// assertions (method/path/header/decoded body).
type loggedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   map[string]any
}

// requestLog records every request a fakeServer handler observed, safe for
// concurrent append/read since httptest.Server may serve on its own
// goroutine.
type requestLog struct {
	mu       sync.Mutex
	requests []loggedRequest
}

func (l *requestLog) record(r *http.Request, body map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests = append(l.requests, loggedRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
}

func (l *requestLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.requests)
}

func (l *requestLog) at(i int) loggedRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.requests[i]
}

// decodeBody reads and JSON-decodes r's body.
func decodeBody(t *testing.T, r *http.Request) map[string]any {
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

// writeJSON writes v as status/JSON to w.
func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode fake response: %v", err)
	}
}

// chatCompletionSuccess builds a minimal, valid OpenAI-compatible
// /chat/completions success response body carrying content as the sole
// choice's message content. usage is included only when either count is
// >= 0; pass -1, -1 to omit the "usage" block entirely (some
// OpenAI-compatible servers do).
func chatCompletionSuccess(model, content string, promptTokens, completionTokens int) map[string]any {
	resp := map[string]any{
		"id":     "chatcmpl-test",
		"object": "chat.completion",
		"model":  model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": content},
				"finish_reason": "stop",
			},
		},
	}
	if promptTokens >= 0 && completionTokens >= 0 {
		resp["usage"] = map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		}
	}
	return resp
}

// chatCompletionError builds an OpenAI-style error envelope.
func chatCompletionError(errType, message string) map[string]any {
	return map[string]any{
		"error": map[string]any{"type": errType, "message": message},
	}
}

// fakeServer starts an httptest.Server whose /v1/chat/completions handler
// is handle — the "/v1" prefix matches the real path shape Ollama and LM
// Studio both serve at (Config.BaseURL = "http://host:port/v1"), exercising
// the same BaseURL+"/chat/completions" join real use depends on. Every
// request is decoded and appended to the returned *requestLog before handle
// runs. t.Cleanup closes the server.
func fakeServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, body map[string]any)) (*httptest.Server, *requestLog) {
	t.Helper()
	log := &requestLog{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body := decodeBody(t, r)
		log.record(r, body)
		handle(w, r, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, log
}

// singleReplyServer returns a fake server that always answers 200 OK with
// reply as the sole message content, using model and fixed token counts.
func singleReplyServer(t *testing.T, model, reply string) (*httptest.Server, *requestLog) {
	t.Helper()
	return fakeServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(t, w, http.StatusOK, chatCompletionSuccess(model, reply, 42, 7))
	})
}

// newTestProvider builds a Provider wired to srv's URL with "/v1" appended
// (see fakeServer), a fake model id and a deterministic step clock.
func newTestProvider(t *testing.T, srv *httptest.Server, opts ...func(*openai.Config)) *openai.Provider {
	t.Helper()
	cfg := openai.Config{
		BaseURL: srv.URL + "/v1",
		Model:   "fake-model",
		Now:     stepClock(10 * time.Millisecond),
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	p, err := openai.New(cfg)
	if err != nil {
		t.Fatalf("openai.New: %v", err)
	}
	return p
}

// newRoundTripProvider builds a Provider whose HTTPClient is backed by
// transport instead of a real listener — for the connection-failure/
// context-cancellation tests only (see roundTripFunc's own doc comment).
func newRoundTripProvider(t *testing.T, transport http.RoundTripper) *openai.Provider {
	t.Helper()
	p, err := openai.New(openai.Config{
		BaseURL:    "http://fake.invalid/v1",
		Model:      "fake-model",
		HTTPClient: &http.Client{Transport: transport},
		Now:        stepClock(10 * time.Millisecond),
	})
	if err != nil {
		t.Fatalf("openai.New: %v", err)
	}
	return p
}

// stepClock returns a clock that advances by step on every call, starting
// at a fixed epoch — deterministic and non-zero, so Usage.Latency
// assertions do not depend on wall-clock timing.
func stepClock(step time.Duration) func() time.Time {
	cur := time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)
	return func() time.Time {
		now := cur
		cur = cur.Add(step)
		return now
	}
}

// samplePrompt is a representative actor.Prompt shared across tests: a
// goal/task pair, one bot message with one available action, and no
// history yet — the same shape actor/anthropic/helpers_test.go's
// samplePrompt uses, so the two providers' test suites are easy to compare.
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

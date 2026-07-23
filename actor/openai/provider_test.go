package openai_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/actor/openai"
)

func TestNew_MissingBaseURL(t *testing.T) {
	_, err := openai.New(openai.Config{Model: "fake-model"})
	if !errors.Is(err, openai.ErrMissingBaseURL) {
		t.Fatalf("New() error = %v, want ErrMissingBaseURL", err)
	}
}

func TestNew_MissingModel(t *testing.T) {
	_, err := openai.New(openai.Config{BaseURL: "http://localhost:11434/v1"})
	if !errors.Is(err, openai.ErrMissingModel) {
		t.Fatalf("New() error = %v, want ErrMissingModel", err)
	}
}

// TestPropose_RequestShape asserts the wire shape of a Propose call: method,
// path, the request body's model/max_tokens/response_format (json_schema,
// strict, with a schema), and that the rendered prompt carries the goal,
// task and observation content a Provider is required to include (see
// actor.Prompt's own doc).
func TestPropose_RequestShape(t *testing.T) {
	srv, log := fakeServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(t, w, http.StatusOK, chatCompletionSuccess("fake-model",
			`{"kind":"send-text","text":"milk, eggs, bread","action_id":"","rationale":"the bot asked what to add"}`,
			42, 7))
	})
	p := newTestProvider(t, srv)
	prompt := samplePrompt()

	proposal, usage, err := p.Propose(context.Background(), prompt)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if got := log.count(); got != 1 {
		t.Fatalf("server received %d requests, want 1", got)
	}
	req := log.at(0)

	if req.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", req.Method)
	}
	if req.Path != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", req.Path)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization header = %q, want empty (no APIKey configured)", got)
	}

	if got := req.Body["model"]; got != "fake-model" {
		t.Errorf("body.model = %v, want fake-model", got)
	}
	if got := req.Body["max_tokens"]; got != float64(openai.DefaultMaxTokens) {
		t.Errorf("body.max_tokens = %v, want %v", got, openai.DefaultMaxTokens)
	}

	format, ok := req.Body["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("body.response_format missing or wrong type: %#v", req.Body["response_format"])
	}
	if format["type"] != "json_schema" {
		t.Errorf("response_format.type = %v, want json_schema", format["type"])
	}
	jsonSchema, ok := format["json_schema"].(map[string]any)
	if !ok {
		t.Fatalf("response_format.json_schema missing or wrong type: %#v", format["json_schema"])
	}
	if jsonSchema["strict"] != true {
		t.Errorf("response_format.json_schema.strict = %v, want true", jsonSchema["strict"])
	}
	if _, ok := jsonSchema["schema"].(map[string]any); !ok {
		t.Fatalf("response_format.json_schema.schema missing or wrong type: %#v", jsonSchema["schema"])
	}

	messages, ok := req.Body["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("body.messages = %#v, want exactly two messages (system, user)", req.Body["messages"])
	}
	systemMsg, _ := messages[0].(map[string]any)
	if systemMsg["role"] != "system" {
		t.Errorf("messages[0].role = %v, want system", systemMsg["role"])
	}
	systemText, _ := systemMsg["content"].(string)
	if !strings.Contains(systemText, "EXACTLY one JSON object") {
		t.Errorf("system prompt does not state the response contract: %q", systemText)
	}

	userMsg, _ := messages[1].(map[string]any)
	if userMsg["role"] != "user" {
		t.Errorf("messages[1].role = %v, want user", userMsg["role"])
	}
	userText, _ := userMsg["content"].(string)
	for _, want := range []string{
		prompt.GoalID, prompt.TaskID, prompt.TaskSuccessCriteria,
		"btn-cancel", "What would you like to add?",
	} {
		if !strings.Contains(userText, want) {
			t.Errorf("rendered user prompt is missing %q:\n%s", want, userText)
		}
	}

	if proposal.Kind != actor.ProposeSendText || proposal.Text != "milk, eggs, bread" {
		t.Errorf("proposal = %+v, want send-text %q", proposal, "milk, eggs, bread")
	}
	if usage.Model != "fake-model" || usage.InputTokens != 42 || usage.OutputTokens != 7 {
		t.Errorf("usage = %+v", usage)
	}
	if usage.Cost != nil {
		t.Errorf("usage.Cost = %v, want nil (this package never estimates cost)", *usage.Cost)
	}
	if usage.Latency <= 0 {
		t.Errorf("usage.Latency = %v, want > 0", usage.Latency)
	}
	if got := p.LastResponseFormatMode(); got != openai.ModeJSONSchema {
		t.Errorf("LastResponseFormatMode() = %q, want %q", got, openai.ModeJSONSchema)
	}
}

// TestPropose_AuthHeaderOnlyWhenAPIKeySet covers both directions: a local
// server (no APIKey configured) gets no Authorization header at all, not an
// empty one; a configured APIKey reaches the wire as a Bearer token.
func TestPropose_AuthHeaderOnlyWhenAPIKeySet(t *testing.T) {
	tests := []struct {
		name       string
		apiKey     string
		wantHeader string
	}{
		{"no API key (local server)", "", ""},
		{"API key set", "sk-test-123", "Bearer sk-test-123"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reply := `{"kind":"give-up","text":"","action_id":"","rationale":"r"}`
			srv, log := singleReplyServer(t, "fake-model", reply)
			p := newTestProvider(t, srv, func(c *openai.Config) { c.APIKey = tc.apiKey })

			if _, _, err := p.Propose(context.Background(), samplePrompt()); err != nil {
				t.Fatalf("Propose: %v", err)
			}
			if got := log.at(0).Header.Get("Authorization"); got != tc.wantHeader {
				t.Errorf("Authorization header = %q, want %q", got, tc.wantHeader)
			}
		})
	}
}

// TestPropose_MapsAllProposalKinds is table-driven over every
// actor.ProposalKind the response contract can express, including that a
// "click" proposal's ObservationSequence is always taken from the prompt's
// current observation, never trusted from the model's reply.
func TestPropose_MapsAllProposalKinds(t *testing.T) {
	prompt := samplePrompt()

	tests := []struct {
		name  string
		reply string
		want  actor.Proposal
	}{
		{
			name:  "send-text",
			reply: `{"kind":"send-text","text":"milk, eggs, bread","action_id":"","rationale":"r1"}`,
			want:  actor.Proposal{Kind: actor.ProposeSendText, Text: "milk, eggs, bread", Rationale: "r1"},
		},
		{
			name:  "click",
			reply: `{"kind":"click","text":"","action_id":"btn-cancel","rationale":"r2"}`,
			want: actor.Proposal{
				Kind: actor.ProposeClick, ActionID: "btn-cancel",
				ObservationSequence: prompt.Observation.Sequence, Rationale: "r2",
			},
		},
		{
			name:  "task-done",
			reply: `{"kind":"task-done","text":"","action_id":"","rationale":"r3"}`,
			want:  actor.Proposal{Kind: actor.ProposeTaskDone, Rationale: "r3"},
		},
		{
			name:  "give-up",
			reply: `{"kind":"give-up","text":"","action_id":"","rationale":"r4"}`,
			want:  actor.Proposal{Kind: actor.ProposeGiveUp, Rationale: "r4"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := singleReplyServer(t, "fake-model", tc.reply)
			p := newTestProvider(t, srv)
			got, _, err := p.Propose(context.Background(), prompt)
			if err != nil {
				t.Fatalf("Propose: %v", err)
			}
			if got != tc.want {
				t.Errorf("Propose() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestPropose_RepairsWrappedJSON exercises the one-reparse-attempt repair
// path: a reply that wraps the JSON object in prose/markdown despite the
// response contract still parses successfully.
func TestPropose_RepairsWrappedJSON(t *testing.T) {
	wrapped := "Sure, here is my choice:\n```json\n" +
		`{"kind":"give-up","text":"","action_id":"","rationale":"stuck in a menu loop"}` +
		"\n```\nHope that helps!"

	srv, _ := singleReplyServer(t, "fake-model", wrapped)
	p := newTestProvider(t, srv)
	got, _, err := p.Propose(context.Background(), samplePrompt())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	want := actor.Proposal{Kind: actor.ProposeGiveUp, Rationale: "stuck in a menu loop"}
	if got != want {
		t.Errorf("Propose() = %+v, want %+v", got, want)
	}
}

// TestPropose_UnparseableReplyNeverFabricatesAProposal covers both an
// unparseable-even-after-repair reply and a schema-violating one (missing
// "kind"): Propose must return a typed *openai.InvalidResponseError and the
// zero-value Proposal, never a guess.
func TestPropose_UnparseableReplyNeverFabricatesAProposal(t *testing.T) {
	tests := []struct {
		name  string
		reply string
	}{
		{"no JSON at all", "I'm not sure how to respond to that."},
		{"truncated JSON", `{"kind":"send-text","text":"partial`},
		{"missing kind", `{"text":"hello","action_id":"","rationale":"oops"}`},
		{"unknown kind", `{"kind":"do-a-barrel-roll","text":"","action_id":"","rationale":"oops"}`},
		{"click without action_id", `{"kind":"click","text":"","action_id":"","rationale":"oops"}`},
		{"send-text without text", `{"kind":"send-text","text":"","action_id":"","rationale":"oops"}`},
		{"empty rationale", `{"kind":"give-up","text":"","action_id":"","rationale":""}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := singleReplyServer(t, "fake-model", tc.reply)
			p := newTestProvider(t, srv)
			got, usage, err := p.Propose(context.Background(), samplePrompt())

			if err == nil {
				t.Fatal("Propose() error = nil, want a non-nil error")
			}
			var invalid *openai.InvalidResponseError
			if !errors.As(err, &invalid) {
				t.Fatalf("Propose() error = %v (%T), want *openai.InvalidResponseError", err, err)
			}
			if got != (actor.Proposal{}) {
				t.Errorf("Propose() returned a non-zero Proposal on parse failure: %+v", got)
			}
			// Usage still reflects that the HTTP call succeeded — only the
			// Proposal is withheld.
			if usage.Model == "" {
				t.Error("usage.Model is empty even though the call succeeded at the HTTP level")
			}
		})
	}
}

// TestPropose_MissingContent covers a response whose first choice has no
// content at all (e.g. a content filter, or a "length" cutoff before any
// text landed): a typed *openai.InvalidResponseError naming finish_reason,
// never a fabricated Proposal.
func TestPropose_MissingContent(t *testing.T) {
	srv, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion", "model": "fake-model",
			"choices": []map[string]any{
				{"index": 0, "message": map[string]any{"role": "assistant", "content": ""}, "finish_reason": "content_filter"},
			},
		})
	})
	p := newTestProvider(t, srv)

	got, _, err := p.Propose(context.Background(), samplePrompt())
	var invalid *openai.InvalidResponseError
	if !errors.As(err, &invalid) {
		t.Fatalf("Propose() error = %v (%T), want *openai.InvalidResponseError", err, err)
	}
	if invalid.FinishReason != "content_filter" {
		t.Errorf("invalid.FinishReason = %q, want content_filter", invalid.FinishReason)
	}
	if got != (actor.Proposal{}) {
		t.Errorf("Propose() fabricated a Proposal on empty content: %+v", got)
	}
}

// chatCompletionSuccessField is like chatCompletionSuccess but writes the
// reply into an arbitrary message field name instead of always "content" —
// used by the reasoning_content/reasoning tests below to build a response
// whose "content" is empty and the reply text lives in a reasoning field
// instead (see chatwright/runtime-go#3).
func chatCompletionSuccessField(model, field, text, finishReason string) map[string]any {
	message := map[string]any{"role": "assistant", "content": ""}
	if field != "" {
		message[field] = text
	}
	return map[string]any{
		"id": "chatcmpl-test", "object": "chat.completion", "model": model,
		"choices": []map[string]any{
			{"index": 0, "message": message, "finish_reason": finishReason},
		},
		"usage": map[string]any{"prompt_tokens": 20, "completion_tokens": 45, "total_tokens": 65},
	}
}

// TestPropose_ReasoningContentFallback_ValidJSON covers a reasoning model
// that routes its ENTIRE structured-output reply into
// message.reasoning_content and leaves message.content empty — the
// LM Studio/DeepSeek-style shape the first actor-model arena run hit with
// qwen/qwen3.6-27b (chatwright/runtime-go#3): billed output tokens, empty
// content, a valid proposal sitting unread in reasoning_content. Propose
// must extract and return the Proposal from that field instead of treating
// the empty content as no reply at all.
func TestPropose_ReasoningContentFallback_ValidJSON(t *testing.T) {
	reply := `{"kind":"give-up","text":"","action_id":"","rationale":"reasoning model routed its reply into reasoning_content"}`
	srv, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(t, w, http.StatusOK, chatCompletionSuccessField("fake-model", "reasoning_content", reply, "stop"))
	})
	p := newTestProvider(t, srv)

	got, usage, err := p.Propose(context.Background(), samplePrompt())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	want := actor.Proposal{Kind: actor.ProposeGiveUp, Rationale: "reasoning model routed its reply into reasoning_content"}
	if got != want {
		t.Errorf("Propose() = %+v, want %+v", got, want)
	}
	// Usage.OutputTokens must still reflect what the server billed, even
	// though the billed text was never in "content" — this package never
	// second-guesses the server's own token accounting.
	if usage.OutputTokens != 45 {
		t.Errorf("usage.OutputTokens = %d, want 45", usage.OutputTokens)
	}
}

// TestPropose_ReasoningFieldFallback_ValidJSON covers the alternate
// "reasoning" field name a minority of other OpenAI-compatible servers use
// for the same purpose, checked only when both content and
// reasoning_content are empty.
func TestPropose_ReasoningFieldFallback_ValidJSON(t *testing.T) {
	reply := `{"kind":"task-done","text":"","action_id":"","rationale":"criteria visibly met"}`
	srv, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(t, w, http.StatusOK, chatCompletionSuccessField("fake-model", "reasoning", reply, "stop"))
	})
	p := newTestProvider(t, srv)

	got, _, err := p.Propose(context.Background(), samplePrompt())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	want := actor.Proposal{Kind: actor.ProposeTaskDone, Rationale: "criteria visibly met"}
	if got != want {
		t.Errorf("Propose() = %+v, want %+v", got, want)
	}
}

// TestPropose_ReasoningFieldGarbage_TypedError covers a reasoning field
// (either name) that is NOT a valid proposal at all — prose, or JSON that
// does not satisfy the response contract. Some reply text having arrived is
// never enough on its own: Propose must still return the typed
// *openai.InvalidResponseError naming which field it came from, never
// fabricate a Proposal.
func TestPropose_ReasoningFieldGarbage_TypedError(t *testing.T) {
	tests := []struct {
		name  string
		field string
	}{
		{"reasoning_content prose, not JSON", "reasoning_content"},
		{"reasoning prose, not JSON", "reasoning"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
				writeJSON(t, w, http.StatusOK, chatCompletionSuccessField("fake-model", tc.field,
					"Let me think about this step by step... the user wants to add items.", "stop"))
			})
			p := newTestProvider(t, srv)

			got, _, err := p.Propose(context.Background(), samplePrompt())
			var invalid *openai.InvalidResponseError
			if !errors.As(err, &invalid) {
				t.Fatalf("Propose() error = %v (%T), want *openai.InvalidResponseError", err, err)
			}
			if got != (actor.Proposal{}) {
				t.Errorf("Propose() fabricated a Proposal from %s garbage: %+v", tc.field, got)
			}
			if invalid.Source != tc.field {
				t.Errorf("invalid.Source = %q, want %q", invalid.Source, tc.field)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error message %q does not name the source field %q", err.Error(), tc.field)
			}
		})
	}
}

// TestPropose_ContentWinsOverReasoningContent proves precedence: when
// message.content is non-empty, message.reasoning_content is never even
// inspected — unchanged behaviour from before this package read reasoning
// fields at all.
func TestPropose_ContentWinsOverReasoningContent(t *testing.T) {
	contentReply := `{"kind":"give-up","text":"","action_id":"","rationale":"from content"}`
	reasoningReply := `{"kind":"task-done","text":"","action_id":"","rationale":"from reasoning_content, must be ignored"}`
	srv, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion", "model": "fake-model",
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]any{
						"role":              "assistant",
						"content":           contentReply,
						"reasoning_content": reasoningReply,
					},
					"finish_reason": "stop",
				},
			},
		})
	})
	p := newTestProvider(t, srv)

	got, _, err := p.Propose(context.Background(), samplePrompt())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	want := actor.Proposal{Kind: actor.ProposeGiveUp, Rationale: "from content"}
	if got != want {
		t.Errorf("Propose() = %+v, want %+v (content must win over reasoning_content)", got, want)
	}
}

// TestPropose_FinishReasonLength_SurfacesInError covers a reply cut off by
// finish_reason "length" — the arena's own trigger for raising
// DefaultMaxTokens (see its doc comment) — whether content landed empty or
// truncated mid-JSON. The typed error must name finish_reason=length and
// call out that the reply was likely truncated, so a developer or the
// loop's own record does not have to guess why parsing failed.
func TestPropose_FinishReasonLength_SurfacesInError(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"empty content", ""},
		{"truncated JSON mid-object", `{"kind":"send-text","text":"partial rea`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
				writeJSON(t, w, http.StatusOK, map[string]any{
					"id": "chatcmpl-test", "object": "chat.completion", "model": "fake-model",
					"choices": []map[string]any{
						{"index": 0, "message": map[string]any{"role": "assistant", "content": tc.content}, "finish_reason": "length"},
					},
				})
			})
			p := newTestProvider(t, srv)

			_, _, err := p.Propose(context.Background(), samplePrompt())
			var invalid *openai.InvalidResponseError
			if !errors.As(err, &invalid) {
				t.Fatalf("Propose() error = %v (%T), want *openai.InvalidResponseError", err, err)
			}
			if invalid.FinishReason != "length" {
				t.Errorf("invalid.FinishReason = %q, want length", invalid.FinishReason)
			}
			msg := err.Error()
			if !strings.Contains(msg, "finish_reason=length") {
				t.Errorf("error message %q does not name finish_reason=length", msg)
			}
			if !strings.Contains(strings.ToLower(msg), "truncat") {
				t.Errorf("error message %q does not explain the truncation", msg)
			}
		})
	}
}

// TestPropose_JSONSchemaRejected_FallsBackToJSONObject covers the graceful-
// degradation path: a server that rejects response_format:"json_schema"
// with a 400 gets exactly one retry with response_format:"json_object" (and
// the schema restated in the system prompt), and a valid reply from that
// retry still produces a correct Proposal.
func TestPropose_JSONSchemaRejected_FallsBackToJSONObject(t *testing.T) {
	srv, log := fakeServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		format, _ := body["response_format"].(map[string]any)
		if format["type"] == "json_schema" {
			writeJSON(t, w, http.StatusBadRequest, chatCompletionError("invalid_request_error", `"response_format" of type "json_schema" is not supported`))
			return
		}
		writeJSON(t, w, http.StatusOK, chatCompletionSuccess("fake-model",
			`{"kind":"give-up","text":"","action_id":"","rationale":"local server has no json_schema support"}`, 10, 4))
	})
	p := newTestProvider(t, srv)

	got, usage, err := p.Propose(context.Background(), samplePrompt())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	want := actor.Proposal{Kind: actor.ProposeGiveUp, Rationale: "local server has no json_schema support"}
	if got != want {
		t.Errorf("Propose() = %+v, want %+v", got, want)
	}
	if usage.InputTokens != 10 || usage.OutputTokens != 4 {
		t.Errorf("usage = %+v, want the fallback response's own counts", usage)
	}

	if got := log.count(); got != 2 {
		t.Fatalf("server received %d requests, want exactly 2 (json_schema then json_object fallback)", got)
	}
	first, second := log.at(0), log.at(1)
	firstFormat, _ := first.Body["response_format"].(map[string]any)
	if firstFormat["type"] != "json_schema" {
		t.Errorf("first request response_format.type = %v, want json_schema", firstFormat["type"])
	}
	secondFormat, _ := second.Body["response_format"].(map[string]any)
	if secondFormat["type"] != "json_object" {
		t.Errorf("second request response_format.type = %v, want json_object", secondFormat["type"])
	}
	messages, _ := second.Body["messages"].([]any)
	systemMsg, _ := messages[0].(map[string]any)
	systemText, _ := systemMsg["content"].(string)
	if !strings.Contains(systemText, `"kind": "send-text|click|task-done|give-up"`) {
		t.Errorf("fallback system prompt does not restate the schema:\n%s", systemText)
	}

	if got := p.LastResponseFormatMode(); got != openai.ModeJSONObjectFallback {
		t.Errorf("LastResponseFormatMode() = %q, want %q", got, openai.ModeJSONObjectFallback)
	}
}

// TestPropose_UnknownRejectionAlsoFallsBack covers the design brief's
// "400/unknown" wording literally: a status this package has no specific
// name for (500, standing in for a server that fails a different way than
// a clean 400) is treated the same as an explicit 400 — still eligible for
// exactly one json_object retry, never classified as auth/rate-limit.
func TestPropose_UnknownRejectionAlsoFallsBack(t *testing.T) {
	srv, log := fakeServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		format, _ := body["response_format"].(map[string]any)
		if format["type"] == "json_schema" {
			writeJSON(t, w, http.StatusInternalServerError, chatCompletionError("server_error", "unsupported request"))
			return
		}
		writeJSON(t, w, http.StatusOK, chatCompletionSuccess("fake-model",
			`{"kind":"task-done","text":"","action_id":"","rationale":"recovered via fallback"}`, 5, 2))
	})
	p := newTestProvider(t, srv)

	got, _, err := p.Propose(context.Background(), samplePrompt())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if got.Kind != actor.ProposeTaskDone {
		t.Errorf("Propose() = %+v, want task-done", got)
	}
	if log.count() != 2 {
		t.Fatalf("server received %d requests, want 2", log.count())
	}
}

// TestPropose_FallbackAlsoFails_ReturnsTypedError covers the case where
// BOTH attempts fail: a typed *openai.FallbackFailedError naming both
// underlying failures, never a fabricated Proposal, and no further retry.
func TestPropose_FallbackAlsoFails_ReturnsTypedError(t *testing.T) {
	srv, log := fakeServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(t, w, http.StatusBadRequest, chatCompletionError("invalid_request_error", "nope"))
	})
	p := newTestProvider(t, srv)

	got, usage, err := p.Propose(context.Background(), samplePrompt())
	if err == nil {
		t.Fatal("Propose() error = nil, want a non-nil error")
	}
	var fallbackErr *openai.FallbackFailedError
	if !errors.As(err, &fallbackErr) {
		t.Fatalf("Propose() error = %v (%T), want *openai.FallbackFailedError", err, err)
	}
	if got != (actor.Proposal{}) || usage != (actor.Usage{}) {
		t.Errorf("Propose() = %+v, %+v, want both zero-value when both attempts fail", got, usage)
	}
	if log.count() != 2 {
		t.Fatalf("server received %d requests, want exactly 2 (no further retry)", log.count())
	}
	if got := p.LastResponseFormatMode(); got != openai.ModeJSONObjectFallback {
		t.Errorf("LastResponseFormatMode() = %q, want %q (the last mode attempted)", got, openai.ModeJSONObjectFallback)
	}
}

// TestPropose_MissingUsageBlock confirms a response with no "usage" key at
// all (some OpenAI-compatible servers omit it) leaves Usage's token counts
// at zero rather than erroring or guessing.
func TestPropose_MissingUsageBlock(t *testing.T) {
	srv, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(t, w, http.StatusOK, chatCompletionSuccess("fake-model",
			`{"kind":"give-up","text":"","action_id":"","rationale":"r"}`, -1, -1))
	})
	p := newTestProvider(t, srv)

	_, usage, err := p.Propose(context.Background(), samplePrompt())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if usage.InputTokens != 0 || usage.OutputTokens != 0 {
		t.Errorf("usage = %+v, want zero token counts when the response has no usage block", usage)
	}
	if usage.Model != "fake-model" {
		t.Errorf("usage.Model = %q, want fake-model", usage.Model)
	}
}

// TestPropose_ModelFallsBackToConfigModel confirms a response that omits
// "model" (or sends an empty one) still leaves Usage.Model populated, from
// Config.Model.
func TestPropose_ModelFallsBackToConfigModel(t *testing.T) {
	srv, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(t, w, http.StatusOK, chatCompletionSuccess("", // empty model field
			`{"kind":"give-up","text":"","action_id":"","rationale":"r"}`, 1, 1))
	})
	p := newTestProvider(t, srv, func(c *openai.Config) { c.Model = "qwen3.6:latest" })

	_, usage, err := p.Propose(context.Background(), samplePrompt())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if usage.Model != "qwen3.6:latest" {
		t.Errorf("usage.Model = %q, want the configured model qwen3.6:latest", usage.Model)
	}
}

// TestPropose_ErrorTaxonomy covers the HTTP-error classification for
// statuses this package has a specific name for: authentication failures
// and rate limits get typed errors, and — unlike a schema rejection — no
// json_object fallback is attempted for either (see openai.retryable's own
// doc comment: changing response_format cannot fix a bad key or a rate
// limit).
func TestPropose_ErrorTaxonomy(t *testing.T) {
	tests := []struct {
		name   string
		status int
		check  func(t *testing.T, err error)
	}{
		{
			name: "unauthorized", status: http.StatusUnauthorized,
			check: func(t *testing.T, err error) {
				var authErr *openai.AuthenticationError
				if !errors.As(err, &authErr) {
					t.Errorf("error = %v (%T), want *openai.AuthenticationError", err, err)
				}
			},
		},
		{
			name: "forbidden", status: http.StatusForbidden,
			check: func(t *testing.T, err error) {
				var authErr *openai.AuthenticationError
				if !errors.As(err, &authErr) {
					t.Errorf("error = %v (%T), want *openai.AuthenticationError", err, err)
				}
			},
		},
		{
			name: "rate limited", status: http.StatusTooManyRequests,
			check: func(t *testing.T, err error) {
				var rateErr *openai.RateLimitError
				if !errors.As(err, &rateErr) {
					t.Errorf("error = %v (%T), want *openai.RateLimitError", err, err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, log := fakeServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
				writeJSON(t, w, tc.status, chatCompletionError("some_error", "boom"))
			})
			p := newTestProvider(t, srv)

			got, usage, err := p.Propose(context.Background(), samplePrompt())
			if err == nil {
				t.Fatal("Propose() error = nil, want a non-nil error")
			}
			tc.check(t, err)
			if got != (actor.Proposal{}) {
				t.Errorf("Propose() fabricated a Proposal on a %d response: %+v", tc.status, got)
			}
			if usage != (actor.Usage{}) {
				t.Errorf("Propose() returned non-zero Usage on a transport-level error: %+v", usage)
			}
			if got := log.count(); got != 1 {
				t.Errorf("server received %d requests, want 1 (no fallback attempt for %d)", got, tc.status)
			}
		})
	}
}

// TestPropose_ConnectionFailure covers a transport that fails before any
// HTTP response at all (e.g. DNS/connection refused) — still a typed,
// wrapped error, never a fabricated Proposal, and no fallback attempt
// (nothing to fall back from).
func TestPropose_ConnectionFailure(t *testing.T) {
	wantErr := errors.New("connection refused")
	calls := 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, wantErr
	})
	p := newRoundTripProvider(t, transport)

	got, usage, err := p.Propose(context.Background(), samplePrompt())
	if err == nil {
		t.Fatal("Propose() error = nil, want a non-nil error")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Propose() error = %v, want it to wrap %v", err, wantErr)
	}
	if got != (actor.Proposal{}) || usage != (actor.Usage{}) {
		t.Errorf("Propose() = %+v, %+v, want both zero-value on connection failure", got, usage)
	}
	if calls != 1 {
		t.Errorf("transport called %d times, want 1 (no fallback attempt on a connection failure)", calls)
	}
}

// TestPropose_ContextCancellation confirms Propose plumbs ctx through to
// the outgoing HTTP request — a real http.Transport aborts an in-flight
// request when its context is cancelled; this asserts our request carries
// that same (already-cancelled) context, and that Propose surfaces the
// resulting error rather than swallowing it.
func TestPropose_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		t.Fatal("outgoing request's context was not the cancelled one Propose was called with")
		return nil, nil
	})
	p := newRoundTripProvider(t, transport)

	_, _, err := p.Propose(ctx, samplePrompt())
	if err == nil {
		t.Fatal("Propose() error = nil, want a non-nil error for a cancelled context")
	}
}

// TestNew_DefaultsAreApplied confirms the documented Config zero-value
// defaults (max tokens) actually reach the wire.
func TestNew_DefaultsAreApplied(t *testing.T) {
	srv, log := singleReplyServer(t, "fake-model", `{"kind":"give-up","text":"","action_id":"","rationale":"r"}`)

	p, err := openai.New(openai.Config{BaseURL: srv.URL + "/v1", Model: "fake-model"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, _, err := p.Propose(context.Background(), samplePrompt()); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	if got := log.at(0).Body["max_tokens"]; got != float64(openai.DefaultMaxTokens) {
		t.Errorf("body.max_tokens = %v, want default %v", got, openai.DefaultMaxTokens)
	}
}

// TestLastResponseFormatMode_ZeroValueBeforeAnyCall confirms the documented
// zero value.
func TestLastResponseFormatMode_ZeroValueBeforeAnyCall(t *testing.T) {
	p, err := openai.New(openai.Config{BaseURL: "http://localhost:11434/v1", Model: "fake-model"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := p.LastResponseFormatMode(); got != "" {
		t.Errorf("LastResponseFormatMode() before any call = %q, want empty", got)
	}
}

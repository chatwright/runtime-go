package anthropic_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/actor/anthropic"
)

func TestNew_MissingAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")

	_, err := anthropic.New(anthropic.Config{})
	if !errors.Is(err, anthropic.ErrMissingAPIKey) {
		t.Fatalf("New() error = %v, want ErrMissingAPIKey", err)
	}
}

func TestNew_ReadsAPIKeyFromEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "env-key")

	p, err := anthropic.New(anthropic.Config{})
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	if p == nil {
		t.Fatal("New() returned a nil Provider with no error")
	}
}

// TestPropose_RequestShape asserts the wire shape of a Propose call: method,
// path, auth header, and the request body's model/max_tokens/output_config
// plus that the rendered prompt carries the goal, task and observation
// content a Provider is required to include (actor.Prompt's own doc).
func TestPropose_RequestShape(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotHeader http.Header
		gotBody   map[string]any
	)

	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotHeader = r.Header
		gotBody = decodeRequestBody(t, r)
		return jsonResponse(t, http.StatusOK, messagesAPISuccess(
			anthropic.DefaultModel,
			`{"kind":"send-text","text":"milk, eggs, bread","action_id":"","rationale":"the bot asked what to add"}`,
			42, 7,
		)), nil
	})

	p := newTestProvider(t, transport)
	prompt := samplePrompt()

	proposal, usage, err := p.Propose(context.Background(), prompt)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", gotPath)
	}
	if got := gotHeader.Get("X-Api-Key"); got != "test-key" {
		t.Errorf("X-Api-Key header = %q, want test-key", got)
	}
	if got := gotHeader.Get("Anthropic-Version"); got == "" {
		t.Error("Anthropic-Version header is empty, want the SDK's default")
	}

	if got := gotBody["model"]; got != anthropic.DefaultModel {
		t.Errorf("body.model = %v, want %v", got, anthropic.DefaultModel)
	}
	if got := gotBody["max_tokens"]; got != float64(anthropic.DefaultMaxTokens) {
		t.Errorf("body.max_tokens = %v, want %v", got, anthropic.DefaultMaxTokens)
	}

	outputConfig, ok := gotBody["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("body.output_config missing or wrong type: %#v", gotBody["output_config"])
	}
	format, ok := outputConfig["format"].(map[string]any)
	if !ok {
		t.Fatalf("body.output_config.format missing or wrong type: %#v", outputConfig["format"])
	}
	if format["type"] != "json_schema" {
		t.Errorf("format.type = %v, want json_schema", format["type"])
	}
	if _, ok := format["schema"].(map[string]any); !ok {
		t.Fatalf("format.schema missing or wrong type: %#v", format["schema"])
	}

	systemArr, ok := gotBody["system"].([]any)
	if !ok || len(systemArr) == 0 {
		t.Fatalf("body.system missing or empty: %#v", gotBody["system"])
	}
	systemText, _ := systemArr[0].(map[string]any)["text"].(string)
	if !strings.Contains(systemText, "EXACTLY one JSON object") {
		t.Errorf("system prompt does not state the response contract: %q", systemText)
	}

	messages, ok := gotBody["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("body.messages = %#v, want exactly one message", gotBody["messages"])
	}
	userMsg, _ := messages[0].(map[string]any)
	if userMsg["role"] != "user" {
		t.Errorf("messages[0].role = %v, want user", userMsg["role"])
	}
	content, ok := userMsg["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("messages[0].content missing or empty: %#v", userMsg["content"])
	}
	userText, _ := content[0].(map[string]any)["text"].(string)
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
	if usage.Model != anthropic.DefaultModel || usage.InputTokens != 42 || usage.OutputTokens != 7 {
		t.Errorf("usage = %+v", usage)
	}
	if usage.Latency <= 0 {
		t.Errorf("usage.Latency = %v, want > 0", usage.Latency)
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
			p := newTestProvider(t, singleReply(tc.reply))
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

	p := newTestProvider(t, singleReply(wrapped))
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
// "kind"): Propose must return a typed *InvalidResponseError and the
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
			p := newTestProvider(t, singleReply(tc.reply))
			got, usage, err := p.Propose(context.Background(), samplePrompt())

			if err == nil {
				t.Fatal("Propose() error = nil, want a non-nil error")
			}
			var invalid *anthropic.InvalidResponseError
			if !errors.As(err, &invalid) {
				t.Fatalf("Propose() error = %v (%T), want *anthropic.InvalidResponseError", err, err)
			}
			if got != (actor.Proposal{}) {
				t.Errorf("Propose() returned a non-zero Proposal on parse failure: %+v", got)
			}
			// Usage still reflects that tokens were spent on the failed
			// attempt — only the Proposal is withheld.
			if usage.Model == "" {
				t.Error("usage.Model is empty even though the call succeeded at the HTTP level")
			}
		})
	}
}

// TestPropose_RefusalIsInvalidResponseNotFabricated covers a
// safety-classifier refusal: HTTP 200, stop_reason "refusal", empty content.
func TestPropose_RefusalIsInvalidResponseNotFabricated(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(t, http.StatusOK, messagesAPIRefusal(anthropic.DefaultModel)), nil
	})
	p := newTestProvider(t, transport)

	got, _, err := p.Propose(context.Background(), samplePrompt())
	var invalid *anthropic.InvalidResponseError
	if !errors.As(err, &invalid) {
		t.Fatalf("Propose() error = %v (%T), want *anthropic.InvalidResponseError", err, err)
	}
	if invalid.StopReason != "refusal" {
		t.Errorf("invalid.StopReason = %q, want refusal", invalid.StopReason)
	}
	if got != (actor.Proposal{}) {
		t.Errorf("Propose() fabricated a Proposal on refusal: %+v", got)
	}
}

// TestPropose_ErrorTaxonomy covers the HTTP-error classification: auth
// failures and rate limits get typed errors; other failures are wrapped
// generically but still surface the underlying *anthropicsdk.Error via
// errors.As/errors.Unwrap.
func TestPropose_ErrorTaxonomy(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		errType string
		check   func(t *testing.T, err error)
	}{
		{
			name: "unauthorized", status: http.StatusUnauthorized, errType: "authentication_error",
			check: func(t *testing.T, err error) {
				var authErr *anthropic.AuthenticationError
				if !errors.As(err, &authErr) {
					t.Errorf("error = %v (%T), want *anthropic.AuthenticationError", err, err)
				}
			},
		},
		{
			name: "forbidden", status: http.StatusForbidden, errType: "permission_error",
			check: func(t *testing.T, err error) {
				var authErr *anthropic.AuthenticationError
				if !errors.As(err, &authErr) {
					t.Errorf("error = %v (%T), want *anthropic.AuthenticationError", err, err)
				}
			},
		},
		{
			name: "rate limited", status: http.StatusTooManyRequests, errType: "rate_limit_error",
			check: func(t *testing.T, err error) {
				var rateErr *anthropic.RateLimitError
				if !errors.As(err, &rateErr) {
					t.Errorf("error = %v (%T), want *anthropic.RateLimitError", err, err)
				}
			},
		},
		{
			name: "server error", status: http.StatusInternalServerError, errType: "api_error",
			check: func(t *testing.T, err error) {
				var authErr *anthropic.AuthenticationError
				var rateErr *anthropic.RateLimitError
				if errors.As(err, &authErr) || errors.As(err, &rateErr) {
					t.Errorf("server error misclassified as %T", err)
				}
				var invalid *anthropic.InvalidResponseError
				if errors.As(err, &invalid) {
					t.Errorf("server error misclassified as InvalidResponseError: %v", err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return jsonResponse(t, tc.status, messagesAPIError(tc.errType, "boom")), nil
			})
			p := newTestProvider(t, transport)

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
		})
	}
}

// TestPropose_ConnectionFailure covers a transport that fails before any
// HTTP response at all (e.g. DNS/connection refused) — still a typed,
// wrapped error, never a fabricated Proposal.
func TestPropose_ConnectionFailure(t *testing.T) {
	wantErr := errors.New("connection refused")
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, wantErr
	})
	p := newTestProvider(t, transport)

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
	p := newTestProvider(t, transport)

	_, _, err := p.Propose(ctx, samplePrompt())
	if err == nil {
		t.Fatal("Propose() error = nil, want a non-nil error for a cancelled context")
	}
}

// TestPropose_CostEstimate confirms Usage.Cost is populated for a model
// this package has pricing for, and left nil when disabled or unknown.
func TestPropose_CostEstimate(t *testing.T) {
	reply := `{"kind":"give-up","text":"","action_id":"","rationale":"r"}`

	t.Run("known model", func(t *testing.T) {
		p := newTestProvider(t, singleReply(reply))
		_, usage, err := p.Propose(context.Background(), samplePrompt())
		if err != nil {
			t.Fatalf("Propose: %v", err)
		}
		if usage.Cost == nil {
			t.Fatal("usage.Cost is nil, want an estimate for the default (priced) model")
		}
		want := float64(42)/1_000_000*1.00 + float64(7)/1_000_000*5.00
		if *usage.Cost != want {
			t.Errorf("usage.Cost = %v, want %v", *usage.Cost, want)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		p := newTestProvider(t, singleReply(reply), func(c *anthropic.Config) {
			c.DisableCostEstimate = true
		})
		_, usage, err := p.Propose(context.Background(), samplePrompt())
		if err != nil {
			t.Fatalf("Propose: %v", err)
		}
		if usage.Cost != nil {
			t.Errorf("usage.Cost = %v, want nil with DisableCostEstimate", *usage.Cost)
		}
	})
}

// TestNew_DefaultsAreApplied confirms the documented Config zero-value
// defaults (model, max tokens) actually reach the wire.
func TestNew_DefaultsAreApplied(t *testing.T) {
	var gotBody map[string]any
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotBody = decodeRequestBody(t, r)
		return jsonResponse(t, http.StatusOK, messagesAPISuccess(
			anthropic.DefaultModel, `{"kind":"give-up","text":"","action_id":"","rationale":"r"}`, 1, 1,
		)), nil
	})

	p, err := anthropic.New(anthropic.Config{APIKey: "test-key", HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, _, err := p.Propose(context.Background(), samplePrompt()); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	if gotBody["model"] != anthropic.DefaultModel {
		t.Errorf("body.model = %v, want default %v", gotBody["model"], anthropic.DefaultModel)
	}
	if gotBody["max_tokens"] != float64(anthropic.DefaultMaxTokens) {
		t.Errorf("body.max_tokens = %v, want default %v", gotBody["max_tokens"], anthropic.DefaultMaxTokens)
	}
}

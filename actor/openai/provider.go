// Package openai is an actor.Provider that speaks the OpenAI-compatible
// chat-completions wire format: the same request/response shape Ollama, LM
// Studio, OpenRouter, vLLM and OpenAI itself expose at
// POST {BaseURL}/chat/completions. One provider covers all of them because
// they share this wire shape — only Config.BaseURL (and, for hosted
// services, Config.APIKey) changes.
//
// It composes with the frozen actor seam exactly like actor/anthropic does
// — see that package for the seam's own doc comment and README for the
// design this mirrors. Providers are dumb transports: this one renders a
// Prompt to a system+user message pair (see prompt.go), asks for exactly
// one JSON object via response_format (structured output where the server
// supports it, with a graceful one-shot fallback otherwise — see below),
// and maps the reply to a Proposal. It never fabricates a Proposal: an
// unparseable or malformed reply is a typed error, not a guess — see
// response.go.
//
// # Structured output and graceful degradation
//
// Propose first asks for response_format: {"type":"json_schema", ...} with
// strict:true — OpenAI's, and increasingly Ollama/LM Studio's,
// structured-output contract, enforced server-side (see prompt.go's
// responseJSONSchema — the SAME proposal JSON contract actor/anthropic's
// own response schema enforces, so a campaign's actor.Prompt→actor.Proposal
// contract does not vary by provider). Some OpenAI-compatible servers
// (older Ollama/LM Studio builds, third-party servers with partial
// compatibility) reject an unrecognised response_format with an HTTP error
// instead. On any such rejection — see wire.go's retryable classification —
// Propose retries the SAME prompt exactly once with
// response_format: {"type":"json_object"} plus the response schema
// restated in the system prompt (jsonObjectFallbackInstructions), and still
// parses the reply strictly through the same one-repair-attempt path. A
// caller can see which mode actually served the last call via
// Provider.LastResponseFormatMode — useful for tests and diagnostics, never
// required for correct use.
//
// # Reasoning models and the reasoning_content field
//
// Some reasoning models served through an OpenAI-compatible endpoint (LM
// Studio and DeepSeek-style servers observed so far) route their entire
// reply — including the proposal JSON the response contract asks for —
// into message.reasoning_content instead of message.content, leaving
// content empty while still billing output tokens for it (see
// chatwright/runtime-go#3: qwen/qwen3.6-27b via LM Studio did this on 4/4
// calls in the first actor-model arena run). Propose reads that text
// rather than treating an empty content as "no reply": see response.go's
// responseText for the exact field precedence (content, then
// reasoning_content, then the alternate name reasoning — each checked only
// when every earlier one is empty; content winning outright whenever it is
// non-empty leaves existing behaviour unchanged). Text recovered from a
// reasoning field is parsed and validated through the exact same strict,
// one-repair-attempt path as content — see response.go — so this package
// still never fabricates a Proposal from reasoning prose that merely looks
// plausible; a reasoning field that does not hold a valid proposal
// surfaces as the same typed *InvalidResponseError, naming which field
// (Source) it came from.
//
// # Usage and cost
//
// Usage.Model, InputTokens and OutputTokens are read from the response;
// InputTokens/OutputTokens are left zero when the server's response carries
// no "usage" block at all (some OpenAI-compatible servers omit it), never
// guessed. Usage.Cost is always left nil: unlike actor/anthropic's dated
// pricing snapshot (a single hosted vendor with a stable published price
// list), this package fronts arbitrary local/self-hosted servers with no
// shared pricing source of truth at all — most of them are free to run
// locally anyway, which is the whole point of this package. A caller that
// wants Usage.Cost populated (e.g. a hosted OpenAI-compatible endpoint with
// a known rate) prices it themselves from InputTokens/OutputTokens after
// Propose returns; the pricing-snapshot mechanism actor/anthropic/pricing.go
// implements is deliberately NOT replicated here.
package openai

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"chatwright.dev/runtime/actor"
)

// DefaultMaxTokens is the default Config.MaxTokens: generous headroom for a
// short rationale plus the fixed JSON scaffolding. Originally 1024, matching
// actor/anthropic's own DefaultMaxTokens; raised to 2048 after the first
// actor-model arena run (chatwright/backstage
// research/model-arena-2026-07-23) observed finish_reason=length truncating
// replies mid-JSON at 1024 across multiple cells — the arena reran every
// cell at 2048 as a fairness fix, and that value is now this package's own
// default too. Config.MaxTokens still overrides it per Provider; see
// InvalidResponseError, which now names finish_reason=length explicitly
// when it is the culprit.
const DefaultMaxTokens = 2048

// ErrMissingBaseURL means New was called with an empty Config.BaseURL.
// Unlike Anthropic's single hosted API, an OpenAI-compatible provider has
// no universal default endpoint — every server (Ollama, LM Studio,
// OpenRouter, ...) lives at its own address, so BaseURL is always required.
var ErrMissingBaseURL = errors.New("actor/openai: no base URL: set Config.BaseURL (e.g. http://localhost:11434/v1 for Ollama)")

// ErrMissingModel means New was called with an empty Config.Model. There is
// no package-level default model: OpenAI-compatible servers each expose
// their own catalogue (Ollama and LM Studio's model ids are whatever the
// developer pulled/loaded locally), so callers always name one explicitly.
var ErrMissingModel = errors.New("actor/openai: no model: set Config.Model")

// ResponseFormatMode names which response_format mode served a Propose
// call — see Provider.LastResponseFormatMode. It is a string type, not an
// int enum, per AGENTS.md's JSON-artefact convention, even though it never
// itself reaches a JSON artefact — kept consistent with actor.ProposalKind
// and friends.
type ResponseFormatMode string

// Response-format modes. See ResponseFormatMode.
const (
	// ModeJSONSchema: the server accepted the primary, structured-output
	// request (response_format: {"type":"json_schema", ...}).
	ModeJSONSchema ResponseFormatMode = "json_schema"
	// ModeJSONObjectFallback: the server rejected ModeJSONSchema and
	// Propose fell back to response_format: {"type":"json_object"} with
	// the schema restated in the system prompt — see the package doc
	// comment's "Structured output and graceful degradation" section.
	ModeJSONObjectFallback ResponseFormatMode = "json_object_fallback"
)

// String renders m for diagnostics and test failure messages.
func (m ResponseFormatMode) String() string { return string(m) }

// Config configures a Provider.
type Config struct {
	// BaseURL is the OpenAI-compatible server's base URL, e.g.
	// "http://localhost:11434/v1" for Ollama or "http://localhost:1234/v1"
	// for LM Studio. Required: New returns ErrMissingBaseURL if empty.
	// Propose POSTs to {BaseURL}/chat/completions; a trailing slash on
	// BaseURL is tolerated.
	BaseURL string

	// Model is the model id the server should use, e.g. "qwen3.6:latest".
	// Required: New returns ErrMissingModel if empty — see ErrMissingModel.
	Model string

	// APIKey authenticates every request via "Authorization: Bearer
	// <APIKey>". Optional: local servers (Ollama, LM Studio) do not need
	// one; when empty, Propose sends no Authorization header at all,
	// rather than an empty or placeholder one.
	APIKey string

	// MaxTokens bounds the model's reply, sent as the request's
	// "max_tokens". <= 0 uses DefaultMaxTokens.
	MaxTokens int

	// HTTPClient issues every HTTP request. Nil uses http.DefaultClient.
	// Tests point this at a fake http.RoundTripper, or run.a real
	// httptest.Server and pass its BaseURL instead, so Propose never
	// touches the network in CI — see provider_test.go.
	HTTPClient *http.Client

	// Now supplies the provider's notion of the current time, used only to
	// measure Usage.Latency around the API call. Nil uses time.Now. Inject
	// a fake clock for deterministic latency assertions in tests.
	Now func() time.Time
}

// Provider is an actor.Provider backed by an OpenAI-compatible
// /chat/completions endpoint. Build one with New. The zero value is not
// usable.
type Provider struct {
	baseURL    string
	model      string
	apiKey     string
	maxTokens  int
	httpClient *http.Client
	now        func() time.Time

	mu       sync.Mutex
	lastMode ResponseFormatMode
}

var _ actor.Provider = (*Provider)(nil)

// New builds a Provider from cfg. It returns ErrMissingBaseURL if
// cfg.BaseURL is empty, or ErrMissingModel if cfg.Model is empty.
func New(cfg Config) (*Provider, error) {
	if cfg.BaseURL == "" {
		return nil, ErrMissingBaseURL
	}
	if cfg.Model == "" {
		return nil, ErrMissingModel
	}

	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	return &Provider{
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		model:      cfg.Model,
		apiKey:     cfg.APIKey,
		maxTokens:  maxTokens,
		httpClient: httpClient,
		now:        now,
	}, nil
}

// LastResponseFormatMode reports which response_format mode served the
// most recently completed Propose call: ModeJSONSchema (the default,
// structured-output request) or ModeJSONObjectFallback (the server
// rejected json_schema and Propose fell back — see the package doc
// comment). The zero value ("") means Propose has not been called yet.
// Safe for concurrent use; a test/diagnostic hook, never required for
// correct use of Provider.
func (p *Provider) LastResponseFormatMode() ResponseFormatMode {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastMode
}

func (p *Provider) setLastMode(m ResponseFormatMode) {
	p.mu.Lock()
	p.lastMode = m
	p.mu.Unlock()
}

// Propose implements actor.Provider: it renders prompt (see prompt.go),
// POSTs it to {BaseURL}/chat/completions for exactly one structured-output
// JSON reply (with the json_object fallback on rejection — see the package
// doc comment), and maps that reply to a Proposal (see response.go). It
// never returns a fabricated Proposal: any failure to obtain and parse a
// valid reply is a typed error (see errors.go), leaving the caller's
// zero-value Proposal untouched. Usage.Cost is always left nil — see the
// package doc comment's "Usage and cost" section.
func (p *Provider) Propose(ctx context.Context, prompt actor.Prompt) (actor.Proposal, actor.Usage, error) {
	system, user := renderPrompt(prompt)

	start := p.now()
	resp, mode, err := p.completeWithFallback(ctx, system, user)
	latency := p.now().Sub(start)
	p.setLastMode(mode)
	if err != nil {
		return actor.Proposal{}, actor.Usage{}, err
	}

	usage := actor.Usage{Model: resp.Model, Latency: latency}
	if usage.Model == "" {
		// The response omitted "model" (some OpenAI-compatible servers
		// do); fall back to the model we asked for rather than leave
		// Usage.Model empty.
		usage.Model = p.model
	}
	if resp.Usage != nil {
		usage.InputTokens = resp.Usage.PromptTokens
		usage.OutputTokens = resp.Usage.CompletionTokens
	}
	// Usage.Cost stays nil unconditionally — see the package doc comment.

	proposal, err := proposalFromResponse(resp, prompt)
	if err != nil {
		return actor.Proposal{}, usage, err
	}
	return proposal, usage, nil
}

// completeWithFallback performs the primary json_schema request and, only
// when the server's failure is classified as retryable (see wire.go's
// retryable), exactly one json_object fallback request with the schema
// restated in the system prompt. It returns which mode actually produced
// the returned response (or was last attempted, on failure) alongside the
// response/error, so Propose can record it via setLastMode regardless of
// outcome.
func (p *Provider) completeWithFallback(ctx context.Context, system, user string) (*chatCompletionResponse, ResponseFormatMode, error) {
	resp, canFallback, err := p.doRequest(ctx, system, user, jsonSchemaResponseFormat())
	if err == nil {
		return resp, ModeJSONSchema, nil
	}
	if !canFallback {
		return nil, ModeJSONSchema, err
	}

	fallbackSystem := system + "\n\n" + jsonObjectFallbackInstructions
	resp2, _, err2 := p.doRequest(ctx, fallbackSystem, user, jsonObjectResponseFormat())
	if err2 != nil {
		return nil, ModeJSONObjectFallback, &FallbackFailedError{JSONSchemaErr: err, JSONObjectErr: err2}
	}
	return resp2, ModeJSONObjectFallback, nil
}

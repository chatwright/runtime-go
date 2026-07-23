package arena

import (
	"context"
	"net/http"
	"sync"
	"time"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/actor/openai"
)

// ProviderKind names which local/hosted server family a ProviderSpec
// targets — the seam DefaultLoaders and the default provider factory key
// off to pick the right eviction/right-sizing mechanism (see Loader) and
// display label. It does not change how Propose calls are made: every kind
// speaks the OpenAI-compatible wire format (see actor/openai's own package
// doc comment), so a server this package has no specific mechanism for
// still runs, as KindOpenAICompat — only the optional eviction/
// right-sizing pre-load step is skipped; the mandatory warm-up call still
// applies.
type ProviderKind string

// Provider kinds. See ProviderKind.
const (
	// KindOllama: a local Ollama server — right-sized/pre-loaded via its
	// native API (see OllamaLoader).
	KindOllama ProviderKind = "ollama"
	// KindLMStudio: a local LM Studio server — right-sized/pre-loaded via
	// the `lms` CLI when present (see LMStudioLoader).
	KindLMStudio ProviderKind = "lmstudio"
	// KindOpenAICompat: any other OpenAI-compatible endpoint (a hosted
	// vendor, OpenRouter, vLLM, ...) — no eviction/right-sizing mechanism.
	KindOpenAICompat ProviderKind = "openai-compat"
)

// ProviderSpec declares one matrix column: a provider/model configuration
// every repeat runs identically.
type ProviderSpec struct {
	// Kind selects the eviction/right-sizing mechanism (see ProviderKind,
	// Loader) and is recorded in the report; it does not change how
	// Propose calls are made.
	Kind ProviderKind
	// Label overrides this column's display name in the report (default:
	// "<Kind>/<Model>", matching the scratchpad harness's report rows).
	Label string
	// BaseURL is the OpenAI-compatible server's base URL, e.g.
	// "http://localhost:11434/v1" for Ollama or "http://localhost:1234/v1"
	// for LM Studio — see actor/openai.Config.BaseURL.
	BaseURL string
	// Model is the model id as the server expects it.
	Model string
	// ContextLength is the context window to load the model with, where
	// the server allows it — the spec's right-sizing rule (spec/ideas/
	// actor-model-arena.md, "founder rule 2026-07-23"). Zero means "let
	// the server pick its own default"; recorded either way in the
	// environment block so entries stay comparable.
	ContextLength int
	// APIKey optionally authenticates every request — see
	// actor/openai.Config.APIKey.
	APIKey string
	// MaxTokens bounds each reply; <= 0 uses actor/openai.DefaultMaxTokens.
	MaxTokens int
}

// label returns s.Label, or the default "<Kind>/<Model>" when unset.
func (s ProviderSpec) label() string {
	if s.Label != "" {
		return s.Label
	}
	return string(s.Kind) + "/" + s.Model
}

// CallRecord is one Provider.Propose call, successful or not — the detail
// a run bundle's LoopEvent has no field for. actor.Loop only appends a
// LoopEvent after a successful Propose call (a Propose error aborts the
// task immediately, before any event is recorded — see actor.Loop.RunTask),
// so a failed call is otherwise invisible to a bundle entirely; the
// required retry breakdown's transport-errors count (spec/ideas/
// actor-model-arena.md) depends on CallRecord for exactly that reason.
// Ported from the scratchpad harness's internal/sidecar.CallRecord, moved
// from a JSON sidecar file into this typed in-memory record — see the
// package doc comment.
type CallRecord struct {
	Index int
	At    time.Time
	Wall  time.Duration
	// Mode is the response_format mode that served this call (see
	// openai.ResponseFormatMode) when the wrapped Provider reports one,
	// empty otherwise (e.g. a ScriptedProvider in a test).
	Mode         string
	TaskID       string
	ProposalKind string
	InputTokens  int
	OutputTokens int
	// Error is the Propose call's own error text, empty on success.
	Error string
}

// responseFormatReporter is implemented by *openai.Provider
// (LastResponseFormatMode) — recordingProvider type-asserts against it so
// CallRecord.Mode is only ever populated for a wrapped Provider that
// actually reports one; a caller-supplied Provider (e.g. a test's
// ScriptedProvider) simply leaves it empty rather than this package
// guessing.
type responseFormatReporter interface {
	LastResponseFormatMode() openai.ResponseFormatMode
}

// recordingProvider decorates any actor.Provider, appending one CallRecord
// per Propose call — the mechanism the scratchpad harness's
// internal/sidecar.RecordingProvider used, generalised from *openai.Provider
// to any actor.Provider so a test can wrap actor.NewScriptedProvider the
// same way a real cell wraps an actor/openai.Provider.
type recordingProvider struct {
	inner actor.Provider
	now   func() time.Time

	mu    sync.Mutex
	calls []CallRecord
}

var _ actor.Provider = (*recordingProvider)(nil)

func newRecordingProvider(inner actor.Provider, now func() time.Time) *recordingProvider {
	return &recordingProvider{inner: inner, now: now}
}

// Propose calls through to r.inner.Propose, recording wall time, the
// response-format mode (when reported — see responseFormatReporter), and
// the error text if any. It never swallows or alters inner's own return
// values.
func (r *recordingProvider) Propose(ctx context.Context, prompt actor.Prompt) (actor.Proposal, actor.Usage, error) {
	start := r.now()
	proposal, usage, err := r.inner.Propose(ctx, prompt)
	wall := r.now().Sub(start)

	rec := CallRecord{
		At: start, Wall: wall, TaskID: prompt.TaskID,
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
	}
	if reporter, ok := r.inner.(responseFormatReporter); ok {
		rec.Mode = reporter.LastResponseFormatMode().String()
	}
	if err != nil {
		rec.Error = err.Error()
	} else {
		rec.ProposalKind = proposal.Kind.String()
	}

	r.mu.Lock()
	rec.Index = len(r.calls)
	r.calls = append(r.calls, rec)
	r.mu.Unlock()

	return proposal, usage, err
}

// Records returns a copy of every CallRecord captured so far, in call
// order.
func (r *recordingProvider) Records() []CallRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]CallRecord(nil), r.calls...)
}

// defaultProviderFactory builds an actor/openai.Provider from spec — the
// default RunOptions.ProviderFactory unless a caller overrides it (e.g. a
// test substituting actor.NewScriptedProvider — see the package's e2e
// test).
func defaultProviderFactory(spec ProviderSpec, httpTimeout time.Duration, now func() time.Time) (actor.Provider, error) {
	return openai.New(openai.Config{
		BaseURL:    spec.BaseURL,
		Model:      spec.Model,
		APIKey:     spec.APIKey,
		MaxTokens:  spec.MaxTokens,
		HTTPClient: &http.Client{Timeout: httpTimeout},
		Now:        now,
	})
}

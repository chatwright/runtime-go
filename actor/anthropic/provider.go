// Package anthropic is the first real actor/actor.Provider implementation:
// it calls the Anthropic Messages API to propose the next action for an
// in-flight campaign task.
//
// It composes with the frozen actor seam like any other Provider — nothing
// in this package changes actor.Provider, actor.Prompt, actor.Proposal,
// actor.Usage or the Loop's semantics. In particular it is meant to be
// wrapped in an actor.CassetteProvider: record once against the live API
// with a real key, commit the cassette under testdata/cassettes/, and CI
// replays it at zero token cost (see README.md).
//
// Providers are dumb transports (see actor's package doc): this one renders
// a Prompt to text, asks the model to reply with exactly one JSON object
// (Anthropic's structured-outputs contract enforces the shape server-side —
// see prompt.go), and maps that reply to a Proposal. It never fabricates a
// Proposal: an unparseable or malformed reply is a typed error, not a
// guess — see response.go.
package anthropic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"chatwright.dev/runtime/actor"
)

// DefaultModel is the default model this package proposes with:
// claude-haiku-4-5, Anthropic's fastest and most cost-effective current
// model (see README.md for the source). A campaign is many small,
// latency-sensitive turns per task — action selection from a short,
// well-structured prompt, not open-ended reasoning — so the fast/cheap tier
// is the right default; callers that want a stronger model for harder
// campaigns set Config.Model explicitly.
const DefaultModel = sdk.ModelClaudeHaiku4_5

// DefaultMaxTokens is the default Config.MaxTokens: generous headroom for a
// short rationale plus the fixed JSON scaffolding, well under the
// ~16000-token non-streaming ceiling, so every call stays a plain
// non-streaming request.
const DefaultMaxTokens = int64(1024)

// apiKeyEnvVar is the environment variable Config.APIKey falls back to when
// left empty. Never read from actor.Prompt, and never written to a cassette
// — see actor.Cassette's doctrine that provider auth lives outside the
// prompt entirely.
const apiKeyEnvVar = "ANTHROPIC_API_KEY"

// ErrMissingAPIKey means New was called with an empty Config.APIKey and the
// ANTHROPIC_API_KEY environment variable was also unset (or empty).
var ErrMissingAPIKey = errors.New("actor/anthropic: no API key: set Config.APIKey or the ANTHROPIC_API_KEY environment variable")

// Config configures a Provider.
type Config struct {
	// APIKey authenticates every request. If empty, New reads the
	// ANTHROPIC_API_KEY environment variable; if that is also empty or
	// unset, New returns ErrMissingAPIKey. Never sourced from an
	// actor.Prompt.
	APIKey string

	// Model is the Anthropic model id to propose with. Empty uses
	// DefaultModel.
	Model string

	// MaxTokens bounds the model's reply. <= 0 uses DefaultMaxTokens.
	MaxTokens int64

	// HTTPClient overrides the HTTP client the Anthropic SDK issues
	// requests with. Nil uses the SDK's default client. Tests set this to
	// an *http.Client backed by a fake http.RoundTripper so Propose never
	// touches the network — see provider_test.go.
	HTTPClient *http.Client

	// BaseURL overrides the Anthropic API base URL. Empty uses the SDK's
	// default (https://api.anthropic.com/). Tests that prefer a real HTTP
	// server over a fake RoundTripper point this at an httptest.Server.
	BaseURL string

	// Now supplies the provider's notion of the current time, used only to
	// measure Usage.Latency around the API call. Nil uses time.Now. Inject
	// a fake clock for deterministic latency assertions in tests.
	Now func() time.Time

	// DisableCostEstimate turns off the automatic Usage.Cost estimate (see
	// pricing.go) even for models this package has pricing for. Usage.Cost
	// is always left nil for models it has no pricing for, regardless of
	// this flag.
	DisableCostEstimate bool

	// MaxRetries overrides the Anthropic SDK's automatic retry count for
	// retryable errors (429, 5xx, connection failures) with exponential
	// backoff. Nil keeps the SDK default (2). Tests set this to a pointer
	// to 0 so an error-taxonomy test asserts on the first response instead
	// of waiting through real backoff delays.
	MaxRetries *int
}

// Provider is an actor.Provider backed by the Anthropic Messages API. Build
// one with New. The zero value is not usable.
type Provider struct {
	client              sdk.Client
	model               string
	maxTokens           int64
	now                 func() time.Time
	disableCostEstimate bool
}

var _ actor.Provider = (*Provider)(nil)

// New builds a Provider from cfg. It returns ErrMissingAPIKey if neither
// cfg.APIKey nor the ANTHROPIC_API_KEY environment variable supplies a key.
func New(cfg Config) (*Provider, error) {
	apiKey := cfg.APIKey
	if apiKey == "" {
		apiKey = os.Getenv(apiKeyEnvVar)
	}
	if apiKey == "" {
		return nil, ErrMissingAPIKey
	}

	model := cfg.Model
	if model == "" {
		model = DefaultModel
	}
	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	opts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.MaxRetries != nil {
		opts = append(opts, option.WithMaxRetries(*cfg.MaxRetries))
	}

	return &Provider{
		client:              sdk.NewClient(opts...),
		model:               model,
		maxTokens:           maxTokens,
		now:                 now,
		disableCostEstimate: cfg.DisableCostEstimate,
	}, nil
}

// Propose implements actor.Provider: it renders prompt (see prompt.go),
// calls the Anthropic Messages API for exactly one structured-output JSON
// reply, and maps that reply to a Proposal (see response.go). It never
// returns a fabricated Proposal — any failure to obtain and parse a valid
// reply is a typed error (see errors.go), leaving the caller's zero-value
// Proposal untouched.
func (p *Provider) Propose(ctx context.Context, prompt actor.Prompt) (actor.Proposal, actor.Usage, error) {
	system, user := renderPrompt(prompt)

	start := p.now()
	resp, err := p.client.Messages.New(ctx, sdk.MessageNewParams{
		Model:     p.model,
		MaxTokens: p.maxTokens,
		System:    []sdk.TextBlockParam{{Text: system}},
		Messages: []sdk.MessageParam{
			sdk.NewUserMessage(sdk.NewTextBlock(user)),
		},
		OutputConfig: sdk.OutputConfigParam{
			Format: sdk.JSONOutputFormatParam{Schema: responseJSONSchema},
		},
	})
	latency := p.now().Sub(start)
	if err != nil {
		return actor.Proposal{}, actor.Usage{}, classifyRequestError(err)
	}

	usage := actor.Usage{
		Model:        resp.Model,
		InputTokens:  int(resp.Usage.InputTokens),
		OutputTokens: int(resp.Usage.OutputTokens),
		Latency:      latency,
	}
	if !p.disableCostEstimate {
		usage.Cost = estimateCost(resp.Model, resp.Usage.InputTokens, resp.Usage.OutputTokens)
	}

	proposal, err := proposalFromResponse(resp, prompt)
	if err != nil {
		return actor.Proposal{}, usage, err
	}
	return proposal, usage, nil
}

// classifyRequestError maps an error from the Anthropic SDK to this
// package's error taxonomy (see errors.go), so callers can distinguish
// "retry later" (rate limit, transient server error) from "fix
// configuration" (authentication) from "network/other" without depending on
// SDK-internal types.
func classifyRequestError(err error) error {
	var apiErr *sdk.Error
	if !errors.As(err, &apiErr) {
		return fmt.Errorf("actor/anthropic: request failed: %w", err)
	}
	switch apiErr.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return &AuthenticationError{Err: apiErr}
	case http.StatusTooManyRequests:
		return &RateLimitError{Err: apiErr}
	default:
		return fmt.Errorf("actor/anthropic: request failed: %w", apiErr)
	}
}

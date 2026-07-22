package anthropic

import "fmt"

// AuthenticationError wraps an Anthropic API 401/403 response: the API key
// is missing, invalid, revoked, or lacks access to the requested model.
// Never retryable without fixing the key.
type AuthenticationError struct{ Err error }

func (e *AuthenticationError) Error() string {
	return fmt.Sprintf("actor/anthropic: authentication failed: %v", e.Err)
}
func (e *AuthenticationError) Unwrap() error { return e.Err }

// RateLimitError wraps an Anthropic API 429 response. Retryable after
// backoff; this package does not retry internally (see README.md) — the
// loop/caller decides whether and when to retry a failed Propose call.
type RateLimitError struct{ Err error }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("actor/anthropic: rate limited: %v", e.Err)
}
func (e *RateLimitError) Unwrap() error { return e.Err }

// InvalidResponseError means the model's reply could not be turned into a
// valid actor.Proposal — malformed JSON even after the one repair attempt
// (see response.go), a JSON object that does not match the response
// contract (missing/invalid "kind", or a kind whose required field is
// empty), or an API response with no text content at all (e.g. a refusal
// with an empty content array). Raw carries the model's raw text (truncated
// for the error message) so a developer can see what went wrong; Propose
// never fabricates a Proposal in its place.
type InvalidResponseError struct {
	// Raw is the model's raw response text that failed to parse, or empty
	// if the response carried no text content at all.
	Raw string
	// StopReason is the API response's stop_reason, when known — e.g.
	// "refusal" or "max_tokens" explain why Raw is empty or truncated.
	StopReason string
	Err        error
}

func (e *InvalidResponseError) Error() string {
	raw := e.Raw
	const maxRawInError = 200
	if len(raw) > maxRawInError {
		raw = raw[:maxRawInError] + "…"
	}
	if e.StopReason != "" {
		return fmt.Sprintf("actor/anthropic: invalid response (stop_reason=%s): %v (raw: %q)", e.StopReason, e.Err, raw)
	}
	return fmt.Sprintf("actor/anthropic: invalid response: %v (raw: %q)", e.Err, raw)
}
func (e *InvalidResponseError) Unwrap() error { return e.Err }

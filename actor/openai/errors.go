package openai

import (
	"fmt"
	"strings"
)

// AuthenticationError wraps an OpenAI-compatible server's 401/403 response:
// the API key is missing, invalid, revoked, or lacks access to the
// requested model. Never retryable without fixing the key — Propose does
// not attempt the json_object fallback for this status (a rejected key is
// not a response_format rejection; see the package doc comment).
type AuthenticationError struct{ Err error }

func (e *AuthenticationError) Error() string {
	return fmt.Sprintf("actor/openai: authentication failed: %v", e.Err)
}
func (e *AuthenticationError) Unwrap() error { return e.Err }

// RateLimitError wraps an OpenAI-compatible server's 429 response.
// Retryable after backoff; this package does not retry internally — the
// loop/caller decides whether and when to retry a failed Propose call. Not
// eligible for the json_object fallback either: a rate limit is not a
// response_format rejection.
type RateLimitError struct{ Err error }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("actor/openai: rate limited: %v", e.Err)
}
func (e *RateLimitError) Unwrap() error { return e.Err }

// InvalidResponseError means the model's reply — from either the
// json_schema request or, after a fallback, the json_object request —
// could not be turned into a valid actor.Proposal: malformed JSON even
// after the one repair attempt (see response.go), a JSON object that does
// not match the response contract (missing/invalid "kind", or a kind whose
// required field is empty), or a response with no usable text in any field
// at all. Raw carries the model's raw reply text (truncated for the error
// message) so a developer can see what went wrong; Propose never
// fabricates a Proposal in its place.
type InvalidResponseError struct {
	// Raw is the model's raw reply text that failed to parse, or empty if
	// the response carried no usable text in any field at all.
	Raw string
	// FinishReason is the response's first choice's finish_reason, when
	// known — e.g. "length" or "content_filter" can explain why Raw is
	// empty or truncated. "length" is called out explicitly in Error()
	// (see the arena evidence in DefaultMaxTokens's doc comment).
	FinishReason string
	// Source names which response field Raw was read from — one of
	// "content" (the normal path), "reasoning_content" or "reasoning" (a
	// reasoning model routed its reply there instead — see response.go's
	// responseText) — or empty when the response carried no usable text in
	// any field (Raw is "" in that case too). Called out in Error() only
	// when it names a reasoning field, since that is the unusual case a
	// developer needs to know about; the normal "content" path is silent
	// exactly as it always was.
	Source string
	Err    error
}

func (e *InvalidResponseError) Error() string {
	raw := e.Raw
	const maxRawInError = 200
	if len(raw) > maxRawInError {
		raw = raw[:maxRawInError] + "…"
	}

	var tags []string
	switch e.FinishReason {
	case "":
		// nothing to tag
	case "length":
		tags = append(tags, "finish_reason=length, reply likely truncated before max_tokens was reached")
	default:
		tags = append(tags, fmt.Sprintf("finish_reason=%s", e.FinishReason))
	}
	if e.Source != "" && e.Source != fieldContent {
		tags = append(tags, fmt.Sprintf("source=%s", e.Source))
	}

	if len(tags) > 0 {
		return fmt.Sprintf("actor/openai: invalid response (%s): %v (raw: %q)", strings.Join(tags, ", "), e.Err, raw)
	}
	return fmt.Sprintf("actor/openai: invalid response: %v (raw: %q)", e.Err, raw)
}
func (e *InvalidResponseError) Unwrap() error { return e.Err }

// FallbackFailedError means the primary json_schema request failed in a
// way this package classified as retryable (see wire.go's retryable), and
// the one-shot json_object fallback attempt also failed at the HTTP/
// transport level (a malformed-but-200 reply from the fallback attempt
// instead surfaces as *InvalidResponseError, not this type — see
// Propose). Both underlying errors are retained so a developer can see
// what the server said both times; neither attempt is retried further.
type FallbackFailedError struct {
	// JSONSchemaErr is the primary (response_format: json_schema)
	// attempt's error.
	JSONSchemaErr error
	// JSONObjectErr is the fallback (response_format: json_object)
	// attempt's error.
	JSONObjectErr error
}

func (e *FallbackFailedError) Error() string {
	return fmt.Sprintf("actor/openai: json_schema request failed (%v), json_object fallback also failed: %v",
		e.JSONSchemaErr, e.JSONObjectErr)
}

// Unwrap supports errors.Is/errors.As against either underlying error, per
// the multi-error Unwrap() []error convention (Go 1.20+).
func (e *FallbackFailedError) Unwrap() []error { return []error{e.JSONSchemaErr, e.JSONObjectErr} }

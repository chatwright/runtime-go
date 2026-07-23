package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// chatCompletionResponse is the subset of an OpenAI-compatible
// POST /chat/completions success response body Propose needs: the first
// choice's message content (the model's reply text) and its finish_reason,
// the model id the server actually served, and, when present, token usage.
// Every OpenAI-compatible server this package targets (OpenAI, Ollama, LM
// Studio, OpenRouter, vLLM) emits at least this much.
type chatCompletionResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	// Usage is a pointer so a response that omits the block entirely (some
	// OpenAI-compatible servers do not send one) is distinguishable from
	// one that sends it with zero counts — either way Propose leaves
	// Usage.InputTokens/OutputTokens at zero, never guessed.
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// errorEnvelope is the OpenAI-style error response body:
// {"error": {"message": ..., "type": ...}}. Servers vary in how faithfully
// they populate it; classifyStatusError falls back to the raw response
// body when it does not parse as this shape.
type errorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// doRequest POSTs one /chat/completions request with the given system/user
// messages and response_format, and returns the parsed success response,
// or an error plus whether that error is eligible for the json_object
// fallback (see retryable) — never both a non-nil response and a non-nil
// error.
func (p *Provider) doRequest(ctx context.Context, system, user string, responseFormat map[string]any) (*chatCompletionResponse, bool, error) {
	reqBody := map[string]any{
		"model": p.model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"response_format": responseFormat,
		"max_tokens":      p.maxTokens,
	}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, false, fmt.Errorf("actor/openai: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return nil, false, fmt.Errorf("actor/openai: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		// A transport-level failure (connection refused, DNS, cancelled
		// context, ...) never reached the server at all — nothing to fall
		// back from, so this is never retryable.
		return nil, false, fmt.Errorf("actor/openai: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("actor/openai: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, retryable(resp.StatusCode), classifyStatusError(resp.StatusCode, body)
	}

	var out chatCompletionResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, false, fmt.Errorf("actor/openai: decode response: %w (body: %s)", err, truncateBody(body))
	}
	return &out, false, nil
}

// retryable reports whether an HTTP failure status is eligible for the
// one-shot json_object fallback (see Provider.completeWithFallback and the
// package doc comment's "Structured output and graceful degradation"
// section): every status except the two this package classifies as
// definitively not fixable by changing response_format — 401/403
// (authentication) and 429 (rate limit). This deliberately treats an
// unrecognised failure status the same as an explicit 400 Bad Request
// ("400/unknown" in the design brief): some OpenAI-compatible servers
// reject an unsupported response_format with a 4xx/5xx status this package
// has no specific name for, and the fallback is harmless to attempt either
// way — a second failure still surfaces as a typed error, never silently
// swallowed.
func retryable(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		return false
	default:
		return true
	}
}

// classifyStatusError maps an HTTP failure status/body to this package's
// error taxonomy (see errors.go).
func classifyStatusError(status int, body []byte) error {
	var env errorEnvelope
	_ = json.Unmarshal(body, &env) // best-effort; env stays zero-value on failure
	msg := env.Error.Message
	if msg == "" {
		msg = truncateBody(body)
	}

	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return &AuthenticationError{Err: fmt.Errorf("status %d: %s", status, msg)}
	case http.StatusTooManyRequests:
		return &RateLimitError{Err: fmt.Errorf("status %d: %s", status, msg)}
	default:
		return fmt.Errorf("actor/openai: request failed: status %d: %s", status, msg)
	}
}

// truncateBody renders body as a bounded string for an error message.
func truncateBody(body []byte) string {
	const maxBodyInError = 200
	s := string(body)
	if len(s) > maxBodyInError {
		return s[:maxBodyInError] + "…"
	}
	return s
}

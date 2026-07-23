package openai_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/actor/openai"
)

// TestLiveLocal_Propose is this package's one optional live smoke test: it
// calls a REAL OpenAI-compatible local server (Ollama, LM Studio, or any
// other compatible endpoint you point it at) with a trivial prompt, and
// checks only that a response comes back as *some* valid Proposal with
// usage attached — not any particular content, which a live model does not
// owe us. Every other behaviour (request shape, parsing, repair, fallback,
// error taxonomy) is covered by the fake-server tests in this package at
// zero cost; this test exists only to catch a real integration break (an
// endpoint shape a real server sends differently than the fake one models)
// that a fake server cannot.
//
// Gated behind CHATWRIGHT_LIVE_LOCAL_LLM=1 AND
// CHATWRIGHT_LOCAL_LLM_BASE_URL, so `go test ./...` never depends on a
// local server being up by default — CI stays green with nothing running.
// CHATWRIGHT_LOCAL_LLM_MODEL is optional; when unset, the test asks the
// server's own GET {baseURL}/models for its catalogue (the same
// OpenAI-compatible listing endpoint Ollama and LM Studio both expose) and
// uses the first model listed.
//
// The founder's own invocation, against Ollama with the chosen local dev
// model pinned explicitly for a repeatable run:
//
//	CHATWRIGHT_LIVE_LOCAL_LLM=1 \
//	CHATWRIGHT_LOCAL_LLM_BASE_URL=http://localhost:11434/v1 \
//	CHATWRIGHT_LOCAL_LLM_MODEL=qwen3.6:latest \
//	  go test ./actor/openai/ -run TestLiveLocal -v
//
// ...or against LM Studio, letting the test discover whichever model LM
// Studio has loaded:
//
//	CHATWRIGHT_LIVE_LOCAL_LLM=1 CHATWRIGHT_LOCAL_LLM_BASE_URL=http://localhost:1234/v1 \
//	  go test ./actor/openai/ -run TestLiveLocal -v
func TestLiveLocal_Propose(t *testing.T) {
	if os.Getenv("CHATWRIGHT_LIVE_LOCAL_LLM") != "1" {
		t.Skip("skipping live local-LLM call: set CHATWRIGHT_LIVE_LOCAL_LLM=1 (and CHATWRIGHT_LOCAL_LLM_BASE_URL) to run it")
	}
	baseURL := os.Getenv("CHATWRIGHT_LOCAL_LLM_BASE_URL")
	if baseURL == "" {
		t.Skip("skipping live local-LLM call: CHATWRIGHT_LOCAL_LLM_BASE_URL is not set")
	}

	model := os.Getenv("CHATWRIGHT_LOCAL_LLM_MODEL")
	if model == "" {
		var err error
		model, err = firstListedModel(baseURL)
		if err != nil {
			t.Fatalf("discover a default model from %s/models: %v", baseURL, err)
		}
	}
	t.Logf("live local LLM: baseURL=%s model=%s", baseURL, model)

	p, err := openai.New(openai.Config{BaseURL: baseURL, Model: model})
	if err != nil {
		t.Fatalf("openai.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	start := time.Now()
	proposal, usage, err := p.Propose(ctx, samplePrompt())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}

	switch proposal.Kind {
	case actor.ProposeSendText, actor.ProposeClick, actor.ProposeTaskDone, actor.ProposeGiveUp:
		// any of these is a legitimate live response to samplePrompt's
		// single bot question with one available action
	default:
		t.Errorf("proposal.Kind = %v, want one of the four known kinds", proposal.Kind)
	}
	if proposal.Rationale == "" {
		t.Error("proposal.Rationale is empty")
	}
	if usage.Model == "" {
		t.Error("usage.Model is empty")
	}
	t.Logf("live response_format mode: %s", p.LastResponseFormatMode())
	t.Logf("live proposal: %+v", proposal)
	t.Logf("live usage: %+v (wall time %s)", usage, elapsed)
}

// firstListedModel asks baseURL's OpenAI-compatible GET /models endpoint
// for its catalogue and returns the first model id listed — the same
// endpoint Ollama and LM Studio both expose. Deliberately test-local, not
// part of Provider's own API surface: Config.Model is always the caller's
// explicit choice in real use (see Config.Model's own doc comment for why
// there is no package-level default) — this is only a convenience for an
// interactive, unattended live-smoke run.
func firstListedModel(baseURL string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s/models: status %d", baseURL, resp.StatusCode)
	}

	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Data) == 0 {
		return "", fmt.Errorf("%s/models listed no models", baseURL)
	}
	return out.Data[0].ID, nil
}

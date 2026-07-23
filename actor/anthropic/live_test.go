package anthropic_test

import (
	"context"
	"os"
	"testing"
	"time"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/actor/anthropic"
)

// TestLive_Propose is the package's one optional live smoke test: it calls
// the real Anthropic API with DefaultModel and a trivial prompt, and checks
// only that the response comes back as *some* valid Proposal with usage
// attached — not any particular content, which a live model does not owe
// us. Every other behaviour (request shape, parsing, repair, error
// taxonomy) is covered by the fake-transport tests above at zero token
// cost; this test exists only to catch a real integration break (wrong
// endpoint, wrong schema shape the live API rejects, auth wiring) that a
// fake transport cannot.
//
// Gated behind CHATWRIGHT_LIVE_LLM=1 AND ANTHROPIC_API_KEY so `go test
// ./...` never spends a token by default — CI stays free. Run locally with:
//
//	CHATWRIGHT_LIVE_LLM=1 ANTHROPIC_API_KEY=sk-ant-... go test ./actor/anthropic/ -run TestLive -v
func TestLive_Propose(t *testing.T) {
	if os.Getenv("CHATWRIGHT_LIVE_LLM") != "1" {
		t.Skip("skipping live Anthropic API call: set CHATWRIGHT_LIVE_LLM=1 (and ANTHROPIC_API_KEY) to run it")
	}
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("skipping live Anthropic API call: ANTHROPIC_API_KEY is not set")
	}

	p, err := anthropic.New(anthropic.Config{})
	if err != nil {
		t.Fatalf("anthropic.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	proposal, usage, err := p.Propose(ctx, samplePrompt())
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
	if usage.InputTokens == 0 || usage.OutputTokens == 0 {
		t.Errorf("usage = %+v, want non-zero token counts from a real call", usage)
	}
	t.Logf("live proposal: %+v", proposal)
	t.Logf("live usage: %+v", usage)
}

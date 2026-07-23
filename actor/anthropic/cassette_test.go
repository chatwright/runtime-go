package anthropic_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/actor/anthropic"
)

// TestCassetteComposition proves a Provider composes with
// actor.CassetteProvider exactly like any other actor.Provider (the frozen
// seam's own promise): recording a call against the fake transport, then
// replaying the same actor.Prompt from the saved cassette reproduces the
// identical Proposal and Usage without a second HTTP call reaching the
// transport at all.
func TestCassetteComposition(t *testing.T) {
	reply := `{"kind":"click","text":"","action_id":"btn-cancel","rationale":"the cancel button is the only sensible next step"}`
	transport := &countingTransport{
		handle: func(r *http.Request) (*http.Response, error) {
			return jsonResponse(t, http.StatusOK, messagesAPISuccess(anthropic.DefaultModel, reply, 11, 3)), nil
		},
	}
	provider := newTestProvider(t, transport)
	prompt := samplePrompt()

	// Record.
	recordCassette := actor.NewCassette("actor/anthropic model=" + anthropic.DefaultModel)
	recorder, err := actor.NewCassetteProvider(actor.ModeRecord, provider, recordCassette)
	if err != nil {
		t.Fatalf("NewCassetteProvider(record): %v", err)
	}

	wantProposal, wantUsage, err := recorder.Propose(context.Background(), prompt)
	if err != nil {
		t.Fatalf("record Propose: %v", err)
	}
	if got := transport.callCount(); got != 1 {
		t.Fatalf("transport calls after recording = %d, want 1", got)
	}

	// Round-trip the cassette through JSON, exactly as actor.Cassette.Save
	// / actor.LoadCassette would across a real testdata/cassettes/*.json
	// file — this is the human-reviewable artifact a real recording session
	// commits (see README.md).
	dir := t.TempDir()
	cassettePath := dir + "/example.json"
	if err := recorder.Cassette().Save(cassettePath); err != nil {
		t.Fatalf("Cassette.Save: %v", err)
	}
	loaded, err := actor.LoadCassette(cassettePath)
	if err != nil {
		t.Fatalf("LoadCassette: %v", err)
	}

	// Replay: wrapped is nil per NewCassetteProvider's own contract for
	// ModeReplay — replay must never fall through to a live call.
	replayer, err := actor.NewCassetteProvider(actor.ModeReplay, nil, loaded)
	if err != nil {
		t.Fatalf("NewCassetteProvider(replay): %v", err)
	}

	gotProposal, gotUsage, err := replayer.Propose(context.Background(), prompt)
	if err != nil {
		t.Fatalf("replay Propose: %v", err)
	}

	if gotProposal != wantProposal {
		t.Errorf("replayed Proposal = %+v, want %+v (recorded)", gotProposal, wantProposal)
	}
	if !usagesEqual(gotUsage, wantUsage) {
		t.Errorf("replayed Usage = %+v, want %+v (recorded)", gotUsage, wantUsage)
	}
	if got := transport.callCount(); got != 1 {
		t.Errorf("transport calls after replay = %d, want still 1 (replay must not touch the network)", got)
	}
}

// TestCassetteComposition_ReplayCacheMiss confirms that replaying against a
// prompt the cassette never recorded is a deterministic cache-miss error
// (actor.ErrCassetteCacheMiss), not a fabricated Proposal and not a live
// call — Provider does not need to know or care that it is being replayed;
// actor.CassetteProvider owns that behaviour entirely.
func TestCassetteComposition_ReplayCacheMiss(t *testing.T) {
	cassette := actor.NewCassette("actor/anthropic model=" + anthropic.DefaultModel)
	replayer, err := actor.NewCassetteProvider(actor.ModeReplay, nil, cassette)
	if err != nil {
		t.Fatalf("NewCassetteProvider(replay): %v", err)
	}

	_, _, err = replayer.Propose(context.Background(), samplePrompt())
	if !errors.Is(err, actor.ErrCassetteCacheMiss) {
		t.Fatalf("Propose() error = %v, want it to wrap actor.ErrCassetteCacheMiss", err)
	}
}

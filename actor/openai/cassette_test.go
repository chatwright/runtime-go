package openai_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"chatwright.dev/runtime/actor"
)

// TestCassetteComposition proves a Provider composes with
// actor.CassetteProvider exactly like any other actor.Provider (the frozen
// seam's own promise, and exactly what actor/anthropic/cassette_test.go
// proves for that provider): recording a call against the fake server,
// then replaying the same actor.Prompt from the saved cassette reproduces
// the identical Proposal and Usage without a second HTTP call reaching the
// server at all.
func TestCassetteComposition(t *testing.T) {
	reply := `{"kind":"click","text":"","action_id":"btn-cancel","rationale":"the cancel button is the only sensible next step"}`
	srv, log := fakeServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(t, w, http.StatusOK, chatCompletionSuccess("fake-model", reply, 11, 3))
	})
	provider := newTestProvider(t, srv)
	prompt := samplePrompt()

	// Record.
	recordCassette := actor.NewCassette("actor/openai model=fake-model")
	recorder, err := actor.NewCassetteProvider(actor.ModeRecord, provider, recordCassette)
	if err != nil {
		t.Fatalf("NewCassetteProvider(record): %v", err)
	}

	wantProposal, wantUsage, err := recorder.Propose(context.Background(), prompt)
	if err != nil {
		t.Fatalf("record Propose: %v", err)
	}
	if got := log.count(); got != 1 {
		t.Fatalf("server requests after recording = %d, want 1", got)
	}

	// Round-trip the cassette through JSON, exactly as actor.Cassette.Save
	// / actor.LoadCassette would across a real testdata/cassettes/*.json
	// file.
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
	if gotUsage != wantUsage {
		t.Errorf("replayed Usage = %+v, want %+v (recorded)", gotUsage, wantUsage)
	}
	if got := log.count(); got != 1 {
		t.Errorf("server requests after replay = %d, want still 1 (replay must not touch the network)", got)
	}
}

// TestCassetteComposition_ReplayCacheMiss confirms that replaying against a
// prompt the cassette never recorded is a deterministic cache-miss error
// (actor.ErrCassetteCacheMiss), not a fabricated Proposal and not a live
// call — Provider does not need to know or care that it is being replayed;
// actor.CassetteProvider owns that behaviour entirely.
func TestCassetteComposition_ReplayCacheMiss(t *testing.T) {
	cassette := actor.NewCassette("actor/openai model=fake-model")
	replayer, err := actor.NewCassetteProvider(actor.ModeReplay, nil, cassette)
	if err != nil {
		t.Fatalf("NewCassetteProvider(replay): %v", err)
	}

	_, _, err = replayer.Propose(context.Background(), samplePrompt())
	if !errors.Is(err, actor.ErrCassetteCacheMiss) {
		t.Fatalf("Propose() error = %v, want it to wrap actor.ErrCassetteCacheMiss", err)
	}
}

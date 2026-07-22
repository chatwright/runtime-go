package actor_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chatwright/chatwright/actor"
	"github.com/chatwright/chatwright/observe"
)

// TestReplayModeFailsOnCacheMiss proves a ModeReplay CassetteProvider never
// falls back to a live call: an unrecorded prompt is a hard error carrying
// the missing prompt's summary, not a silent gap.
func TestReplayModeFailsOnCacheMiss(t *testing.T) {
	cassette := actor.NewCassette("model=test-v1")
	provider, err := actor.NewCassetteProvider(actor.ModeReplay, nil, cassette)
	if err != nil {
		t.Fatalf("NewCassetteProvider() error = %v", err)
	}

	prompt := actor.Prompt{GoalID: "listus", TaskID: "add-items", Observation: observe.Observation{Sequence: 1}}
	_, _, err = provider.Propose(context.Background(), prompt)
	if !errors.Is(err, actor.ErrCassetteCacheMiss) {
		t.Fatalf("Propose() error = %v, want ErrCassetteCacheMiss", err)
	}
	if !strings.Contains(err.Error(), "add-items") {
		t.Fatalf("cache-miss error %q does not carry the missing prompt's summary (task id)", err.Error())
	}
}

// TestCassetteFixtureReplaysFromTestdata replays a real, checked-in example
// cassette under testdata/cassettes/ — demonstrating the design decision
// that cassettes are human-readable JSON files at that path, reviewable in a
// PR diff, not just an in-memory mechanism.
func TestCassetteFixtureReplaysFromTestdata(t *testing.T) {
	cassette, err := actor.LoadCassette("testdata/cassettes/example.json")
	if err != nil {
		t.Fatalf("LoadCassette() error = %v", err)
	}
	replay, err := actor.NewCassetteProvider(actor.ModeReplay, nil, cassette)
	if err != nil {
		t.Fatalf("NewCassetteProvider() error = %v", err)
	}

	prompt := actor.Prompt{
		GoalID: "listus-shopping-list", GoalTitle: "Exercise the shopping-list lifecycle",
		TaskID: "add-items", TaskTitle: "Add several items to the list",
		TaskSuccessCriteria: "several items visible in the list",
		Observation: observe.Observation{
			Sequence: 2,
			Messages: []observe.VisibleMessage{
				{ID: "msg1", Text: "What would you like to add?", Actor: observe.ActorBot},
			},
		},
	}
	proposal, usage, err := replay.Propose(context.Background(), prompt)
	if err != nil {
		t.Fatalf("Propose() error = %v", err)
	}
	if proposal.Kind != actor.ProposeSendText || proposal.Text != "milk, eggs, bread" {
		t.Fatalf("replayed proposal = %+v, want the fixture's send-text \"milk, eggs, bread\"", proposal)
	}
	if usage.Model != "example-scripted-v1" {
		t.Fatalf("replayed usage.Model = %q, want %q", usage.Model, "example-scripted-v1")
	}
}

// TestCassetteRoundTripIsDeterministic proves the whole record/replay
// mechanism: hashing a provider config + prompt is deterministic (same
// inputs, same key, independent of which Cassette instance computed it),
// different prompts key differently, and a cassette recorded, saved,
// reloaded from disk and replayed reproduces the exact recorded
// Proposal/Usage — the property that makes "evidence-backed" claims
// re-examinable in CI at zero token cost.
func TestCassetteRoundTripIsDeterministic(t *testing.T) {
	ctx := context.Background()
	prompt := actor.Prompt{
		GoalID: "listus", TaskID: "add-items",
		Observation: observe.Observation{Sequence: 3, Messages: []observe.VisibleMessage{
			{ID: "msg1", Text: "What would you like to add?", Actor: observe.ActorBot},
		}},
	}
	otherPrompt := prompt
	otherPrompt.TaskID = "checkout"

	usage := actor.Usage{Model: "scripted-v1", InputTokens: 10, OutputTokens: 5}
	proposal := actor.Proposal{Kind: actor.ProposeSendText, Text: "milk", Rationale: "the task asks to add an item"}

	newRecorder := func() *actor.CassetteProvider {
		t.Helper()
		live := actor.NewScriptedProvider(usage, proposal)
		cp, err := actor.NewCassetteProvider(actor.ModeRecord, live, actor.NewCassette("cfg-v1"))
		if err != nil {
			t.Fatalf("NewCassetteProvider() error = %v", err)
		}
		return cp
	}

	rec1 := newRecorder()
	if _, _, err := rec1.Propose(ctx, prompt); err != nil {
		t.Fatalf("rec1.Propose() error = %v", err)
	}
	rec2 := newRecorder()
	if _, _, err := rec2.Propose(ctx, prompt); err != nil {
		t.Fatalf("rec2.Propose() error = %v", err)
	}
	entries1, entries2 := rec1.Cassette().Entries, rec2.Cassette().Entries
	if len(entries1) != 1 || len(entries2) != 1 {
		t.Fatalf("recorded %d and %d entries, want 1 each", len(entries1), len(entries2))
	}
	if entries1[0].Key != entries2[0].Key {
		t.Fatalf("identical provider config + prompt hashed to different keys across two Cassette instances: %q vs %q",
			entries1[0].Key, entries2[0].Key)
	}

	rec3 := newRecorder()
	if _, _, err := rec3.Propose(ctx, otherPrompt); err != nil {
		t.Fatalf("rec3.Propose() error = %v", err)
	}
	if rec3.Cassette().Entries[0].Key == entries1[0].Key {
		t.Fatal("two different prompts hashed to the same cassette key")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "add-items.json")
	if err := rec1.Cassette().Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	reloaded, err := actor.LoadCassette(path)
	if err != nil {
		t.Fatalf("LoadCassette() error = %v", err)
	}
	if reloaded.ProviderConfig != "cfg-v1" {
		t.Fatalf("reloaded ProviderConfig = %q, want %q", reloaded.ProviderConfig, "cfg-v1")
	}

	replay, err := actor.NewCassetteProvider(actor.ModeReplay, nil, reloaded)
	if err != nil {
		t.Fatalf("NewCassetteProvider(replay) error = %v", err)
	}
	gotProposal, gotUsage, err := replay.Propose(ctx, prompt)
	if err != nil {
		t.Fatalf("replay Propose() error = %v", err)
	}
	if gotProposal != proposal {
		t.Fatalf("replayed proposal = %+v, want %+v", gotProposal, proposal)
	}
	if gotUsage.Model != usage.Model || gotUsage.InputTokens != usage.InputTokens || gotUsage.OutputTokens != usage.OutputTokens {
		t.Fatalf("replayed usage = %+v, want %+v", gotUsage, usage)
	}

	// Human-readable, PR-reviewable: the proposal kind renders as text, not
	// a bare enum int.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(raw), `"send-text"`) {
		t.Fatalf("cassette JSON does not render the proposal kind as readable text:\n%s", raw)
	}
}

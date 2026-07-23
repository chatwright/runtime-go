package arena_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/arena"
	"chatwright.dev/runtime/examples/greetbot"
	"chatwright.dev/runtime/observe"
	"chatwright.dev/runtime/platform"
	"chatwright.dev/runtime/telegram"
)

// TestRunScriptedGreetbotEndToEnd is the arena package's CI replay gate
// (spec/ideas/actor-model-arena.md): a 1-model x 1-repeat Matrix driven by
// actor.NewScriptedProvider instead of a real network/CLI provider — zero
// tokens, fully reproducible — against the built-in GreetbotScenario,
// asserting Run produces a valid bundle plus a report containing the rows
// the spec's metric list requires. Mirrors run's own frozen
// TestScriptedCampaignBundleAgainstGreetbotEndToEnd, one layer up: the same
// ScriptedProvider-against-real-greetbot flow, driven through this
// package's Matrix/Run API instead of hand-assembled run.Run/bundle code.
func TestRunScriptedGreetbotEndToEnd(t *testing.T) {
	englishActionID := dryRunLearnEnglishActionID(t)

	// script's first entry is consumed by the mandatory warm-up call (see
	// arena.Run's package doc comment: RunOptions.ProviderFactory is
	// called once per matrix column and reused for both the warm-up and
	// every timed repeat) — its content is irrelevant, since runWarmup
	// never acts on the returned Proposal, only measures the call. The
	// remaining four entries are the scenario's actual script: open the
	// picker, pick English, acknowledge, declare done.
	script := actor.NewScriptedProvider(
		// Zero tokens — the spec's own e2e requirement — since a scripted
		// provider needs no model to report usage for.
		actor.Usage{Model: "scripted-v1"},
		actor.Proposal{Kind: actor.ProposeSendText, Text: "(warmup)", Rationale: "mandatory untimed warm-up call"},
		actor.Proposal{Kind: actor.ProposeSendText, Text: "/start", Rationale: "open the language picker"},
		actor.Proposal{Kind: actor.ProposeClick, ActionID: englishActionID, ObservationSequence: 1, Rationale: "pick English"},
		actor.Proposal{Kind: actor.ProposeSendText, Text: "Thanks!", Rationale: "acknowledge the greeting"},
		actor.Proposal{Kind: actor.ProposeTaskDone, Rationale: "acknowledged, done"},
	)

	matrix := arena.Matrix{
		Scenario: arena.GreetbotScenario(),
		Providers: []arena.ProviderSpec{
			{Kind: arena.KindOpenAICompat, Label: "scripted/v1", Model: "scripted-v1"},
		},
		Repeats: 1,
	}
	opts := arena.RunOptions{
		Now:             func() time.Time { return time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC) },
		ProviderFactory: func(arena.ProviderSpec) (actor.Provider, error) { return script, nil },
	}

	results, err := arena.Run(context.Background(), matrix, opts)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if len(results.Models) != 1 {
		t.Fatalf("len(results.Models) = %d, want 1", len(results.Models))
	}
	model := results.Models[0]
	if model.ProviderErr != nil {
		t.Fatalf("model.ProviderErr = %v", model.ProviderErr)
	}
	if model.Warmup == nil {
		t.Fatal("model.Warmup is nil, want the mandatory warm-up result recorded")
	}
	if model.Warmup.Err != nil {
		t.Fatalf("model.Warmup.Err = %v", model.Warmup.Err)
	}
	if len(model.Cells) != 1 {
		t.Fatalf("len(model.Cells) = %d, want 1", len(model.Cells))
	}

	cell := model.Cells[0]
	if cell.Err != nil {
		t.Fatalf("cell.Err = %v", cell.Err)
	}
	if cell.TaskStatus != "completed" {
		t.Fatalf("cell.TaskStatus = %q, want %q", cell.TaskStatus, "completed")
	}
	if cell.StopReason != "goal-complete" {
		t.Fatalf("cell.StopReason = %q, want %q", cell.StopReason, "goal-complete")
	}
	if !cell.Verified {
		t.Fatalf("cell.Verified = false, want true (VerifyDetail=%q) — the scripted script should satisfy the scenario's own journal-level evidence check", cell.VerifyDetail)
	}
	if cell.BundleName == "" {
		t.Fatal("cell.BundleName is empty")
	}
	for _, call := range cell.Calls {
		if call.Error != "" {
			t.Errorf("unexpected transport error in a fully-scripted, zero-token cell: %q", call.Error)
		}
	}
	if cell.InputTokens != 0 || cell.OutputTokens != 0 {
		// actor.NewScriptedProvider's Usage is reported verbatim (zero
		// here) — the spec's "1-model x 1-repeat matrix ... (zero
		// tokens)" e2e requirement.
		t.Errorf("cell.InputTokens/OutputTokens = %d/%d, want 0/0 (zero-token scripted run)", cell.InputTokens, cell.OutputTokens)
	}

	// A valid bundle: one run, one populated ai-goal part.
	if len(cell.Bundle.Runs) != 1 {
		t.Fatalf("len(cell.Bundle.Runs) = %d, want 1", len(cell.Bundle.Runs))
	}
	bundleRun := cell.Bundle.Runs[0]
	if len(bundleRun.Parts) != 1 || bundleRun.Parts[0].AIGoal == nil {
		t.Fatalf("bundleRun.Parts = %+v, want one populated ai-goal part", bundleRun.Parts)
	}
	if cell.Bundle.Format == "" {
		t.Error("cell.Bundle.Format is empty")
	}

	// The report contains the rows the spec's metric list requires.
	var report strings.Builder
	if err := arena.WriteReport(&report, results); err != nil {
		t.Fatalf("WriteReport() error = %v", err)
	}
	got := report.String()
	for _, want := range []string{
		"scripted/v1",
		"## Environment",
		"## Headline comparison",
		"Cold-start",
		"## Retry breakdown",
		"## Structured-output mode",
		"## Per-cell detail",
		"## Per-model narrative",
		cell.BundleName,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q:\n%s", want, got)
		}
	}
}

// dryRunLearnEnglishActionID mirrors run's own
// TestScriptedCampaignBundleAgainstGreetbotEndToEnd helper: it drives
// greetbot's /start through a throwaway, independent Telegram emulator to
// learn the real observe AvailableAction ID for the "English" button, so
// the test's ScriptedProvider can be built with it up front —
// ScriptedProvider ignores the Prompt it is given (see its own doc
// comment: "it is a fixed sequence, not a policy"), so it cannot discover
// this itself.
func dryRunLearnEnglishActionID(t *testing.T) string {
	t.Helper()
	const chatID = int64(42)
	user := platform.User{ID: 7, FirstName: "Arena"}

	emu := telegram.NewEmulator()
	defer emu.Close()
	bot := greetbot.New(emu.BotAPIURL(), "TEST:TOKEN")
	srv := httptest.NewServer(bot.Handler())
	defer srv.Close()
	emu.SetWebhook(srv.URL, http.DefaultClient)

	if err := emu.SubmitText(chatID, user, "/start"); err != nil {
		t.Fatalf("dry run SubmitText() error = %v", err)
	}

	engine := observe.NewEngine(emu, observe.ChatRef{ChatID: chatID})
	obs, err := engine.Observe()
	if err != nil {
		t.Fatalf("dry run Observe() error = %v", err)
	}
	for _, m := range obs.Messages {
		for _, a := range m.Actions {
			if a.Label == "English" {
				return a.ID
			}
		}
	}
	t.Fatalf("dry run found no \"English\" action among %+v", obs.Messages)
	return ""
}

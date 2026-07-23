package arena_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/arena"
)

// TestRunWritesFlightLogPhaseSequence proves arena.Run, given a
// RunOptions.FlightLog, writes exactly the phase-line vocabulary
// runtime-go#8 requires — in the order Run actually executes them — for a
// 1-model x 1-repeat scripted matrix: the same zero-token, fully
// reproducible harness TestRunScriptedGreetbotEndToEnd uses (see its own
// doc comment for why the script's first entry is consumed by the
// mandatory warm-up call).
func TestRunWritesFlightLogPhaseSequence(t *testing.T) {
	englishActionID := dryRunLearnEnglishActionID(t)

	script := actor.NewScriptedProvider(
		actor.Usage{Model: "scripted-v1"},
		actor.Proposal{Kind: actor.ProposeSendText, Text: "(warmup)", Rationale: "mandatory untimed warm-up call"},
		actor.Proposal{Kind: actor.ProposeSendText, Text: "/start", Rationale: "open the language picker"},
		actor.Proposal{Kind: actor.ProposeClick, ActionID: englishActionID, ObservationSequence: 1, Rationale: "pick English"},
		actor.Proposal{Kind: actor.ProposeSendText, Text: "Thanks!", Rationale: "acknowledge the greeting"},
		actor.Proposal{Kind: actor.ProposeTaskDone, Rationale: "acknowledged, done"},
	)

	path := filepath.Join(t.TempDir(), "flight.log")
	flightLog, err := arena.OpenFlightLog(path)
	if err != nil {
		t.Fatalf("OpenFlightLog() error = %v", err)
	}

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
		FlightLog:       flightLog,
	}

	if _, err := arena.Run(context.Background(), matrix, opts); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if err := flightLog.Close(); err != nil {
		t.Fatalf("flightLog.Close() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read flight log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")

	var phases []string
	for _, line := range lines {
		// Each line is "<RFC3339Nano timestamp> arena: phase=<name> ...".
		idx := strings.Index(line, "phase=")
		if idx < 0 {
			t.Fatalf("flight log line has no phase= field: %q", line)
		}
		rest := line[idx+len("phase="):]
		if sp := strings.IndexByte(rest, ' '); sp >= 0 {
			phases = append(phases, rest[:sp])
		} else {
			phases = append(phases, rest)
		}
	}

	// The memory snapshot line is best-effort (darwin-only, tooling-
	// dependent — see memorySnapshot) so it is filtered out before
	// comparing; every other phase is mandatory and must appear in exactly
	// this order.
	var filtered []string
	for _, p := range phases {
		if p != "memory" {
			filtered = append(filtered, p)
		}
	}

	want := []string{
		"matrix-start",
		"loader-invoke",
		"load",
		"warmup-start",
		"warmup-end",
		"cell-start",
		"cell-end",
		"block-end",
		"matrix-end",
	}
	if len(filtered) != len(want) {
		t.Fatalf("phase sequence = %v, want %v", filtered, want)
	}
	for i, wantPhase := range want {
		if filtered[i] != wantPhase {
			t.Errorf("phases[%d] = %q, want %q (full sequence: %v)", i, filtered[i], wantPhase, filtered)
		}
	}

	// Spot-check the fields the issue specifically calls for: scenario
	// id/version and provider list at matrix start, the model+ctx on the
	// load line, repeat index on the cell lines, cold-start seconds on the
	// warm-up-end line.
	got := string(data)
	for _, want := range []string{
		"scenario=greetbot-language-onboarding@v1",
		"providers=[scripted/v1]",
		"model=scripted/v1",
		"ctx=0",
		"repeat=1",
		"cold_start_s=",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("flight log does not contain %q:\n%s", want, got)
		}
	}
}

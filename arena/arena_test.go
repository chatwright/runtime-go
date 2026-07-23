package arena

import (
	"context"
	"errors"
	"testing"
	"time"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/goal"
)

func TestRunRejectsInvalidMatrix(t *testing.T) {
	validScenario := Scenario{Setup: func() (*ScenarioSession, error) { return nil, errors.New("unused") }}

	cases := []struct {
		name   string
		matrix Matrix
	}{
		{"no scenario setup", Matrix{Providers: []ProviderSpec{{Model: "x"}}, Repeats: 1}},
		{"no providers", Matrix{Scenario: validScenario, Repeats: 1}},
		{"zero repeats", Matrix{Scenario: validScenario, Providers: []ProviderSpec{{Model: "x"}}, Repeats: 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Run(context.Background(), tc.matrix, RunOptions{}); err == nil {
				t.Fatal("Run() error = nil, want a validation error")
			}
		})
	}
}

// TestRunRecordsProviderErrAndContinues proves a ProviderFactory failure
// for one matrix column never aborts the whole matrix — the spec's
// exclusion policy: a model is recorded as a data point (ModelResult.
// ProviderErr), and Run moves on to the next declared provider.
func TestRunRecordsProviderErrAndContinues(t *testing.T) {
	scenario := GreetbotScenario()

	matrix := Matrix{
		Scenario: scenario,
		Providers: []ProviderSpec{
			{Kind: KindOpenAICompat, Model: "broken"},
			{Kind: KindOpenAICompat, Model: "also-broken"},
		},
		Repeats: 1,
	}

	calls := 0
	opts := RunOptions{
		Now: fixedClock,
		ProviderFactory: func(spec ProviderSpec) (actor.Provider, error) {
			calls++
			return nil, errors.New("boom: " + spec.Model)
		},
	}

	results, err := Run(context.Background(), matrix, opts)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (a provider failure is a recorded data point, not a Run error)", err)
	}
	if calls != 2 {
		t.Fatalf("ProviderFactory called %d times, want 2 (one per declared provider)", calls)
	}
	if len(results.Models) != 2 {
		t.Fatalf("len(results.Models) = %d, want 2 — every declared provider must get a ModelResult", len(results.Models))
	}
	for i, m := range results.Models {
		if m.ProviderErr == nil {
			t.Errorf("Models[%d].ProviderErr = nil, want an error", i)
		}
		if m.Warmup != nil {
			t.Errorf("Models[%d].Warmup = %+v, want nil (no provider means no warm-up)", i, m.Warmup)
		}
		if len(m.Cells) != 0 {
			t.Errorf("Models[%d].Cells = %v, want none", i, m.Cells)
		}
	}
}

// TestRunRecordsCellErrAndContinuesRepeats proves a single repeat's setup
// failure is recorded on that CellResult without aborting the rest of that
// provider's repeats.
func TestRunRecordsCellErrAndContinuesRepeats(t *testing.T) {
	attempt := 0
	scenario := Scenario{
		ID: "flaky", Version: "v1", Goal: goal.Goal{ID: "g", Tasks: []goal.Task{{ID: "t", SuccessCriteria: "x"}}},
		Setup: func() (*ScenarioSession, error) {
			attempt++
			if attempt == 1 {
				return nil, errors.New("setup failed on first attempt")
			}
			return GreetbotScenario().Setup()
		},
	}

	matrix := Matrix{
		Scenario:  scenario,
		Providers: []ProviderSpec{{Kind: KindOpenAICompat, Model: "scripted"}},
		Repeats:   2,
	}
	opts := RunOptions{
		Now: fixedClock,
		ProviderFactory: func(ProviderSpec) (actor.Provider, error) {
			return actor.NewScriptedProvider(actor.Usage{}, actor.Proposal{Kind: actor.ProposeGiveUp}), nil
		},
	}

	results, err := Run(context.Background(), matrix, opts)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(results.Models) != 1 {
		t.Fatalf("len(results.Models) = %d, want 1", len(results.Models))
	}
	cells := results.Models[0].Cells
	if len(cells) != 2 {
		t.Fatalf("len(cells) = %d, want 2 (both repeats attempted despite the first failing)", len(cells))
	}
	if cells[0].Err == nil {
		t.Error("cells[0].Err = nil, want the setup error")
	}
	if cells[1].Err != nil {
		t.Errorf("cells[1].Err = %v, want nil (the second repeat's setup succeeds)", cells[1].Err)
	}
}

func TestEffectiveGoalOverridesBudgetsOnlyWhenNonZero(t *testing.T) {
	scenario := Scenario{Goal: goal.Goal{ID: "g", Budgets: goal.Budgets{MaxSteps: 12, MaxDuration: 4 * time.Minute}}}

	// Zero override leaves the scenario's own budgets untouched.
	g := effectiveGoal(scenario, goal.Budgets{})
	if g.Budgets.MaxSteps != 12 {
		t.Errorf("MaxSteps = %d, want 12 (scenario's own budget, unmodified)", g.Budgets.MaxSteps)
	}

	// A non-zero override replaces them entirely.
	g2 := effectiveGoal(scenario, goal.Budgets{MaxSteps: 5})
	if g2.Budgets.MaxSteps != 5 {
		t.Errorf("MaxSteps = %d, want 5 (Matrix.Budgets override)", g2.Budgets.MaxSteps)
	}
	if g2.Budgets.MaxDuration != 0 {
		t.Errorf("MaxDuration = %v, want 0 (override replaces the whole Budgets value, not merged field-by-field)", g2.Budgets.MaxDuration)
	}

	// The scenario's own Goal is never mutated by effectiveGoal.
	if scenario.Goal.Budgets.MaxSteps != 12 {
		t.Errorf("scenario.Goal.Budgets.MaxSteps = %d, want 12 (unmutated)", scenario.Goal.Budgets.MaxSteps)
	}
}

func fixedClock() time.Time { return time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC) }

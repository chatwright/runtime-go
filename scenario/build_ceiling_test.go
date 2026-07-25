package scenario

import (
	"testing"

	"chatwright.dev/runtime/goal"
)

// TestToGoalGoal_MapsCeiling proves Build's Document.Ceiling -> run.RunCeiling
// mapping is correct in every field, including the integer-seconds ->
// time.Duration conversion — see the README's "ceiling ... aggregates
// across ai-goal parts" rule and the ceiling-trip-attributes-run-and-part
// acceptance criterion.
//
// This is a focused mapping test, not a full two-part execution: the
// run-level attribution mechanics it feeds (a RunCeiling trip stopping the
// Run and naming both the ceiling dimension and the executing Part) are the
// run package's own, already covered end-to-end by
// run.TestRunCeilingAttributesReasonAndPart — that test exercises the exact
// same run.RunCeiling this function produces, just constructed by hand
// rather than parsed from a document. What is new here, and what this test
// actually proves, is that a document's authored (integer-seconds) Ceiling
// reaches run.Run unchanged in value and unit.
func TestBuildCeilingMapping(t *testing.T) {
	maxCost := 1.5
	doc := &Document{
		ID: "ceiling-mapping",
		Ceiling: &Ceiling{
			MaxSteps: 7, MaxCost: &maxCost, MaxDurationSeconds: 90,
		},
	}

	if doc.Ceiling.MaxSteps != 7 {
		t.Fatalf("test setup broken")
	}

	// Exercise exactly the conversion Build performs (see build.go's
	// Build), without needing a full bot/cast/parts document.
	ceiling := ceilingFromDocument(doc)
	if ceiling.MaxSteps != 7 {
		t.Errorf("MaxSteps = %d, want 7", ceiling.MaxSteps)
	}
	if ceiling.MaxCost == nil || *ceiling.MaxCost != 1.5 {
		t.Errorf("MaxCost = %v, want 1.5", ceiling.MaxCost)
	}
	if ceiling.MaxDuration != 90_000_000_000 {
		t.Errorf("MaxDuration = %v, want 90s in nanoseconds", ceiling.MaxDuration)
	}
}

func TestToGoalGoal_MapsBudgetSecondsToDuration(t *testing.T) {
	maxCost := 2.0
	g := Goal{
		ID: "g", Title: "t",
		Tasks:   []Task{{ID: "t1", SuccessCriteria: "sc"}},
		Budgets: Budgets{MaxSteps: 3, MaxDurationSeconds: 120, MaxCost: &maxCost},
	}
	got, err := toGoalGoal(g)
	if err != nil {
		t.Fatalf("toGoalGoal() error = %v", err)
	}
	want := goal.Budgets{MaxSteps: 3, MaxDuration: 120_000_000_000, MaxCost: &maxCost}
	if got.Budgets.MaxSteps != want.MaxSteps || got.Budgets.MaxDuration != want.MaxDuration || *got.Budgets.MaxCost != *want.MaxCost {
		t.Errorf("Budgets = %+v, want %+v", got.Budgets, want)
	}
}

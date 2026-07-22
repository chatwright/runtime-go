package goal

import (
	"errors"
	"testing"
)

func TestGoalValidationRejectsDependencyCycle(t *testing.T) {
	g := Goal{
		ID: "cyclic",
		Tasks: []Task{
			{ID: "onboarding", DependsOn: []string{"add-items"}},
			{ID: "add-items", DependsOn: []string{"onboarding"}},
		},
	}
	err := g.Validate()
	if !errors.Is(err, ErrDependencyCycle) {
		t.Fatalf("Validate() error = %v, want ErrDependencyCycle", err)
	}
}

func TestGoalValidationRejectsUnknownDependency(t *testing.T) {
	g := Goal{
		ID: "dangling",
		Tasks: []Task{
			{ID: "add-items", DependsOn: []string{"onboarding"}}, // "onboarding" is never declared
		},
	}
	err := g.Validate()
	if !errors.Is(err, ErrUnknownDependency) {
		t.Fatalf("Validate() error = %v, want ErrUnknownDependency", err)
	}
}

func TestGoalValidationRejectsSelfDependency(t *testing.T) {
	// A task depending on itself is a one-node cycle, not an unknown
	// dependency — it must be caught by cycle detection.
	g := Goal{
		ID:    "self",
		Tasks: []Task{{ID: "loop", DependsOn: []string{"loop"}}},
	}
	err := g.Validate()
	if !errors.Is(err, ErrDependencyCycle) {
		t.Fatalf("Validate() error = %v, want ErrDependencyCycle", err)
	}
}

func TestGoalValidationRejectsDuplicateAndEmptyTaskIDs(t *testing.T) {
	tests := map[string]struct {
		tasks   []Task
		wantErr error
	}{
		"duplicate id": {
			tasks:   []Task{{ID: "add-items"}, {ID: "add-items"}},
			wantErr: ErrDuplicateTaskID,
		},
		"empty id": {
			tasks:   []Task{{ID: "  "}},
			wantErr: ErrEmptyTaskID,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			g := Goal{ID: "g", Tasks: tt.tasks}
			if err := g.Validate(); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestGoalValidationRejectsInvalidBudgets(t *testing.T) {
	negativeCost := -1.0
	zeroCost := 0.0
	tests := map[string]struct {
		budgets Budgets
		wantErr error
	}{
		"negative max steps":         {Budgets{MaxSteps: -1}, ErrNegativeBudget},
		"negative max duration":      {Budgets{MaxDuration: -1}, ErrNegativeBudget},
		"negative repeated failures": {Budgets{MaxRepeatedFailures: -1}, ErrNegativeBudget},
		"zero max cost":              {Budgets{MaxCost: &zeroCost}, ErrNonPositiveCostBudget},
		"negative max cost":          {Budgets{MaxCost: &negativeCost}, ErrNonPositiveCostBudget},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			g := Goal{ID: "g", Tasks: []Task{{ID: "only"}}, Budgets: tt.budgets}
			if err := g.Validate(); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestGoalValidationAcceptsWellFormedGoal(t *testing.T) {
	cost := 5.0
	g := Goal{
		ID:          "listus-shopping-list",
		Title:       "Exercise the shopping-list lifecycle",
		Description: "Register a new user and exercise the shopping list end to end.",
		Constraints: []string{"stay within the isolated test environment"},
		Tasks: []Task{
			{ID: "onboarding", SuccessCriteria: "user completes language selection", Milestones: []string{"onboarding-complete"}},
			{ID: "add-items", DependsOn: []string{"onboarding"}, SuccessCriteria: "several items visible in the list", Milestones: []string{"items-added"}},
			{ID: "remove-items", DependsOn: []string{"add-items"}, SuccessCriteria: "list is empty again"},
		},
		Budgets: Budgets{MaxSteps: 80, MaxDuration: 0, MaxRepeatedFailures: 3, MaxCost: &cost},
	}
	if err := g.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

func TestGoalValidationAcceptsDiamondDependencies(t *testing.T) {
	// A diamond (two independent paths converging) is a valid DAG, not a
	// cycle — make sure the detector doesn't false-positive on it.
	g := Goal{
		ID: "diamond",
		Tasks: []Task{
			{ID: "start"},
			{ID: "left", DependsOn: []string{"start"}},
			{ID: "right", DependsOn: []string{"start"}},
			{ID: "end", DependsOn: []string{"left", "right"}},
		},
	}
	if err := g.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

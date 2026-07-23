package goal

import (
	"errors"
	"testing"
	"time"
)

// fakeClock is an injectable, manually advanced clock so duration-budget
// behaviour is deterministic in tests — CampaignState never calls
// time.Now itself.
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func mustCampaign(t *testing.T, g Goal, now func() time.Time) *CampaignState {
	t.Helper()
	campaign, err := NewCampaignState(g, now)
	if err != nil {
		t.Fatalf("NewCampaignState() error = %v", err)
	}
	return campaign
}

func onboardingAddItemsGoal() Goal {
	return Goal{
		ID: "listus",
		Tasks: []Task{
			{ID: "onboarding", SuccessCriteria: "user completes language selection"},
			{ID: "add-items", DependsOn: []string{"onboarding"}, SuccessCriteria: "several items visible"},
			// An independent third task keeps the campaign from reaching
			// StopGoalComplete while a test still has guard behaviour to
			// exercise on onboarding/add-items.
			{ID: "cleanup", SuccessCriteria: "list is emptied at the end of the run"},
		},
	}
}

func TestTaskStatusTransitionsAreGuarded(t *testing.T) {
	clock := newFakeClock()
	campaign := mustCampaign(t, onboardingAddItemsGoal(), clock.now)

	// A task whose dependency has not completed is not eligible yet.
	if err := campaign.Activate("add-items"); !errors.Is(err, ErrTaskNotEligible) {
		t.Fatalf("Activate(add-items) before dependency = %v, want ErrTaskNotEligible", err)
	}
	if eligible, err := campaign.Eligible("add-items"); err != nil || eligible {
		t.Fatalf("Eligible(add-items) = %v, %v, want false, nil", eligible, err)
	}

	// An unknown task id is always rejected.
	if err := campaign.Activate("does-not-exist"); !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("Activate(unknown) = %v, want ErrUnknownTask", err)
	}

	// Terminalizing a task that was never activated is guarded.
	if err := campaign.Complete("add-items"); !errors.Is(err, ErrTaskNotActive) {
		t.Fatalf("Complete(add-items) while pending = %v, want ErrTaskNotActive", err)
	}

	// Completing the dependency makes the dependent task eligible.
	if err := campaign.Activate("onboarding"); err != nil {
		t.Fatalf("Activate(onboarding) error = %v", err)
	}
	if err := campaign.Complete("onboarding"); err != nil {
		t.Fatalf("Complete(onboarding) error = %v", err)
	}
	if eligible, err := campaign.Eligible("add-items"); err != nil || !eligible {
		t.Fatalf("Eligible(add-items) after dependency completed = %v, %v, want true, nil", eligible, err)
	}

	// Activating an already-terminal task is guarded.
	if err := campaign.Activate("onboarding"); !errors.Is(err, ErrTaskNotActivatable) {
		t.Fatalf("Activate(onboarding) after completion = %v, want ErrTaskNotActivatable", err)
	}

	// Double-activating a task is guarded (no active -> active edge).
	if err := campaign.Activate("add-items"); err != nil {
		t.Fatalf("Activate(add-items) error = %v", err)
	}
	if err := campaign.Activate("add-items"); !errors.Is(err, ErrTaskNotActivatable) {
		t.Fatalf("Activate(add-items) while already active = %v, want ErrTaskNotActivatable", err)
	}

	// A terminal task cannot be re-terminalized.
	if err := campaign.Complete("add-items"); err != nil {
		t.Fatalf("Complete(add-items) error = %v", err)
	}
	if err := campaign.Fail("add-items"); !errors.Is(err, ErrTaskNotActive) {
		t.Fatalf("Fail(add-items) after completion = %v, want ErrTaskNotActive", err)
	}
}

// TestCompleteByEvidenceUsesGoalMetByEvidenceStopReason proves
// CompleteByEvidence behaves exactly like Complete (same guards, same
// TaskCompleted target) but, when the completed task is the one that
// leaves every task terminal, stops the campaign with
// StopGoalMetByEvidence instead of StopGoalComplete — the loop's
// evidence-defined-completion signal, distinct from the actor's own
// task-done claim (see actor.Loop's evidence-defined completion and
// TestGoalCompleteStopReasonIsUnaffectedByEvidenceMethod below).
func TestCompleteByEvidenceUsesGoalMetByEvidenceStopReason(t *testing.T) {
	clock := newFakeClock()
	g := Goal{ID: "g", Tasks: []Task{{ID: "only"}}}
	campaign := mustCampaign(t, g, clock.now)

	if err := campaign.Activate("only"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	if err := campaign.CompleteByEvidence("only"); err != nil {
		t.Fatalf("CompleteByEvidence() error = %v", err)
	}

	status, err := campaign.TaskStatus("only")
	if err != nil || status != TaskCompleted {
		t.Fatalf("TaskStatus() = %v, %v, want TaskCompleted, nil", status, err)
	}
	if !campaign.Stopped() {
		t.Fatal("campaign did not stop once its only task completed")
	}
	if reason, ok := campaign.StopReason(); !ok || reason != StopGoalMetByEvidence {
		t.Fatalf("StopReason() = %v, %v, want StopGoalMetByEvidence, true", reason, ok)
	}
}

// TestGoalCompleteStopReasonIsUnaffectedByEvidenceMethod proves the
// pre-existing Complete path is entirely unchanged: completing the last
// task via the actor's own claim still stops the campaign with the
// pre-existing StopGoalComplete, never StopGoalMetByEvidence.
func TestGoalCompleteStopReasonIsUnaffectedByEvidenceMethod(t *testing.T) {
	clock := newFakeClock()
	g := Goal{ID: "g", Tasks: []Task{{ID: "only"}}}
	campaign := mustCampaign(t, g, clock.now)

	if err := campaign.Activate("only"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	if err := campaign.Complete("only"); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if reason, ok := campaign.StopReason(); !ok || reason != StopGoalComplete {
		t.Fatalf("StopReason() = %v, %v, want StopGoalComplete, true", reason, ok)
	}
}

// TestCompleteByEvidenceDoesNotStopCampaignForNonFinalTask proves
// CompleteByEvidence on a task that is NOT the last one to finish leaves
// the campaign running — the StopGoalMetByEvidence reason is only ever
// used when this completion is what exhausts the campaign's remaining
// work, exactly mirroring Complete's own behaviour.
func TestCompleteByEvidenceDoesNotStopCampaignForNonFinalTask(t *testing.T) {
	clock := newFakeClock()
	g := Goal{ID: "g", Tasks: []Task{{ID: "first"}, {ID: "second"}}}
	campaign := mustCampaign(t, g, clock.now)

	if err := campaign.Activate("first"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	if err := campaign.CompleteByEvidence("first"); err != nil {
		t.Fatalf("CompleteByEvidence() error = %v", err)
	}
	if campaign.Stopped() {
		t.Fatal("campaign stopped after only one of two tasks completed")
	}
}

func TestBudgetsProduceDeterministicStopReasons(t *testing.T) {
	tests := map[string]struct {
		drive func(t *testing.T, clock *fakeClock, campaign *CampaignState)
		want  StopReason
	}{
		"steps budget": {
			drive: func(t *testing.T, _ *fakeClock, campaign *CampaignState) {
				t.Helper()
				if err := campaign.RecordStep(); err != nil {
					t.Fatalf("RecordStep() error = %v", err)
				}
				if err := campaign.RecordStep(); err != nil {
					t.Fatalf("RecordStep() error = %v", err)
				}
			},
			want: StopBudgetSteps,
		},
		"duration budget": {
			drive: func(t *testing.T, clock *fakeClock, campaign *CampaignState) {
				t.Helper()
				clock.advance(6 * time.Minute)
				if err := campaign.RecordStep(); err != nil {
					t.Fatalf("RecordStep() error = %v", err)
				}
			},
			want: StopBudgetDuration,
		},
		"cost budget": {
			drive: func(t *testing.T, _ *fakeClock, campaign *CampaignState) {
				t.Helper()
				if err := campaign.RecordCost(0.6); err != nil {
					t.Fatalf("RecordCost() error = %v", err)
				}
				if campaign.Stopped() {
					t.Fatal("campaign stopped before the cost budget was reached")
				}
				if err := campaign.RecordCost(0.5); err != nil {
					t.Fatalf("RecordCost() error = %v", err)
				}
				if got := campaign.Cost(); got != 1.1 {
					t.Fatalf("Cost() = %v, want 1.1", got)
				}
			},
			want: StopBudgetCost,
		},
		"goal complete": {
			drive: func(t *testing.T, _ *fakeClock, campaign *CampaignState) {
				t.Helper()
				if err := campaign.Activate("onboarding"); err != nil {
					t.Fatalf("Activate() error = %v", err)
				}
				if err := campaign.Complete("onboarding"); err != nil {
					t.Fatalf("Complete() error = %v", err)
				}
			},
			want: StopGoalComplete,
		},
		"cancelled": {
			drive: func(t *testing.T, _ *fakeClock, campaign *CampaignState) {
				t.Helper()
				if err := campaign.Cancel(); err != nil {
					t.Fatalf("Cancel() error = %v", err)
				}
			},
			want: StopCancelled,
		},
		"error": {
			drive: func(t *testing.T, _ *fakeClock, campaign *CampaignState) {
				t.Helper()
				if err := campaign.Abort(); err != nil {
					t.Fatalf("Abort() error = %v", err)
				}
			},
			want: StopError,
		},
	}

	maxCost := 1.0
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			clock := newFakeClock()
			g := Goal{
				ID:      "single-task",
				Tasks:   []Task{{ID: "onboarding"}},
				Budgets: Budgets{MaxSteps: 2, MaxDuration: 5 * time.Minute, MaxCost: &maxCost},
			}
			campaign := mustCampaign(t, g, clock.now)

			tt.drive(t, clock, campaign)

			if !campaign.Stopped() {
				t.Fatal("campaign did not stop")
			}
			got, ok := campaign.StopReason()
			if !ok || got != tt.want {
				t.Fatalf("StopReason() = %v, %v, want %v, true", got, ok, tt.want)
			}

			// Once stopped, further mutation is refused deterministically.
			if err := campaign.RecordStep(); !errors.Is(err, ErrCampaignStopped) {
				t.Fatalf("RecordStep() after stop = %v, want ErrCampaignStopped", err)
			}
		})
	}
}

func TestRepeatedFailureBudgetStopsCampaign(t *testing.T) {
	clock := newFakeClock()
	g := Goal{
		ID: "listus",
		Tasks: []Task{
			{ID: "onboarding"},
			{ID: "add-items", DependsOn: []string{"onboarding"}},
		},
		Budgets: Budgets{MaxRepeatedFailures: 3},
	}
	campaign := mustCampaign(t, g, clock.now)
	if err := campaign.Activate("onboarding"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}

	// Two failures against the same task stay under budget.
	for i := 0; i < 2; i++ {
		if err := campaign.RecordFailure("onboarding"); err != nil {
			t.Fatalf("RecordFailure() #%d error = %v", i+1, err)
		}
	}
	if campaign.Stopped() {
		t.Fatal("campaign stopped before the repeated-failure budget was reached")
	}
	if got := campaign.FailureCount("onboarding"); got != 2 {
		t.Fatalf("FailureCount(onboarding) = %d, want 2", got)
	}

	// A failure against a different task does not count toward
	// onboarding's budget.
	if err := campaign.RecordFailure("add-items"); err != nil {
		t.Fatalf("RecordFailure(add-items) error = %v", err)
	}
	if campaign.Stopped() {
		t.Fatal("campaign stopped from an unrelated task's failure")
	}

	// The third repeated failure against the same task reaches the budget.
	if err := campaign.RecordFailure("onboarding"); err != nil {
		t.Fatalf("RecordFailure() #3 error = %v", err)
	}
	if !campaign.Stopped() {
		t.Fatal("campaign did not stop at the repeated-failure budget")
	}
	if reason, ok := campaign.StopReason(); !ok || reason != StopRepeatedFailure {
		t.Fatalf("StopReason() = %v, %v, want StopRepeatedFailure, true", reason, ok)
	}

	// Once stopped, further mutation on any task is refused.
	if err := campaign.RecordFailure("onboarding"); !errors.Is(err, ErrCampaignStopped) {
		t.Fatalf("RecordFailure() after stop = %v, want ErrCampaignStopped", err)
	}
}

func TestNewCampaignStateRejectsNilClock(t *testing.T) {
	_, err := NewCampaignState(onboardingAddItemsGoal(), nil)
	if !errors.Is(err, ErrNilClock) {
		t.Fatalf("NewCampaignState(nil clock) error = %v, want ErrNilClock", err)
	}
}

func TestNewCampaignStateRejectsInvalidGoal(t *testing.T) {
	clock := newFakeClock()
	invalid := Goal{ID: "bad", Tasks: []Task{{ID: "a", DependsOn: []string{"missing"}}}}
	_, err := NewCampaignState(invalid, clock.now)
	if !errors.Is(err, ErrUnknownDependency) {
		t.Fatalf("NewCampaignState(invalid goal) error = %v, want ErrUnknownDependency", err)
	}
}

func TestRecordCostRejectsNegativeAmount(t *testing.T) {
	clock := newFakeClock()
	campaign := mustCampaign(t, onboardingAddItemsGoal(), clock.now)
	if err := campaign.RecordCost(-0.01); !errors.Is(err, ErrNegativeCost) {
		t.Fatalf("RecordCost(-0.01) error = %v, want ErrNegativeCost", err)
	}
	if campaign.Stopped() {
		t.Fatal("a rejected negative cost must not stop the campaign")
	}
}

func TestCampaignRejectsMutationsAfterStop(t *testing.T) {
	clock := newFakeClock()
	campaign := mustCampaign(t, onboardingAddItemsGoal(), clock.now)
	if err := campaign.Cancel(); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}

	mutations := map[string]func() error{
		"Activate":      func() error { return campaign.Activate("onboarding") },
		"Complete":      func() error { return campaign.Complete("onboarding") },
		"Fail":          func() error { return campaign.Fail("onboarding") },
		"Block":         func() error { return campaign.Block("onboarding") },
		"Skip":          func() error { return campaign.Skip("onboarding") },
		"RecordStep":    campaign.RecordStep,
		"RecordFailure": func() error { return campaign.RecordFailure("onboarding") },
		"RecordCost":    func() error { return campaign.RecordCost(1) },
		"Cancel":        campaign.Cancel,
		"Abort":         campaign.Abort,
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			if err := mutate(); !errors.Is(err, ErrCampaignStopped) {
				t.Fatalf("%s() after stop = %v, want ErrCampaignStopped", name, err)
			}
		})
	}
}

func TestCampaignSnapshotIsDetached(t *testing.T) {
	clock := newFakeClock()
	campaign := mustCampaign(t, onboardingAddItemsGoal(), clock.now)
	if err := campaign.Activate("onboarding"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	if err := campaign.RecordFailure("onboarding"); err != nil {
		t.Fatalf("RecordFailure() error = %v", err)
	}

	snap := campaign.Snapshot()
	snap.Statuses["onboarding"] = TaskCompleted // mutate the copy
	snap.Failures["onboarding"] = 99

	if status, err := campaign.TaskStatus("onboarding"); err != nil || status != TaskActive {
		t.Fatalf("TaskStatus(onboarding) after mutating snapshot = %v, %v, want TaskActive, nil", status, err)
	}
	if got := campaign.FailureCount("onboarding"); got != 1 {
		t.Fatalf("FailureCount(onboarding) after mutating snapshot = %d, want 1", got)
	}
	if snap.GoalID != "listus" {
		t.Fatalf("snapshot GoalID = %q, want %q", snap.GoalID, "listus")
	}
	if snap.Stopped {
		t.Fatal("snapshot reports stopped for a running campaign")
	}
}

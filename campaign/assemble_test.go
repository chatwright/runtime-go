package campaign_test

import (
	"testing"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/campaign"
	"chatwright.dev/runtime/goal"
	"chatwright.dev/runtime/observe"
)

// fourTaskGoal returns a Goal with one task in each outcome this test suite
// exercises: completed cleanly, failed with a stale-proposal history,
// failed cleanly (no such history), and never attempted.
func fourTaskGoal() goal.Goal {
	return goal.Goal{
		ID: "listus", Title: "Exercise the shopping-list lifecycle",
		Tasks: []goal.Task{
			{ID: "onboarding", Title: "Complete onboarding"},
			{ID: "add-items", Title: "Add items", SuccessCriteria: "several items visible"},
			{ID: "checkout", Title: "Checkout"},
			{ID: "cleanup", Title: "Empty the list"},
		},
	}
}

// TestReportClassifiesFindings proves Assemble's three mechanical
// classifications: a task that failed with a stale/invalid proposal history
// becomes ai-navigation-failure; a task never attempted becomes
// coverage-gap; a cleanly completed task produces no finding at all. It also
// proves the caller-supplied verified-defect seam passes a finding through
// untouched, and that a cleanly failed task (no stale history) produces no
// mechanical finding on its own — only AssembleInput.CallerFindings can
// classify it.
func TestReportClassifiesFindings(t *testing.T) {
	events := []actor.LoopEvent{
		{Index: 0, TaskID: "onboarding", ObservationSequence: 1, Action: actor.ActionOutcome{Kind: actor.ActionExecuted}},
		{Index: 1, TaskID: "onboarding", ObservationSequence: 2, Action: actor.ActionOutcome{Kind: actor.ActionTaskCompleted}},

		{Index: 2, TaskID: "add-items", ObservationSequence: 3,
			Validation: actor.ValidationOutcome{Checked: true, Verdict: observe.VerdictStale, Reason: "action no longer available"},
			Action:     actor.ActionOutcome{Kind: actor.ActionSkippedInvalid, Detail: "stale"}},
		{Index: 3, TaskID: "add-items", ObservationSequence: 3, Action: actor.ActionOutcome{Kind: actor.ActionTaskGivenUp}},

		{Index: 4, TaskID: "checkout", ObservationSequence: 4, Action: actor.ActionOutcome{Kind: actor.ActionExecutedNoEffect}},
		{Index: 5, TaskID: "checkout", ObservationSequence: 4, Action: actor.ActionOutcome{Kind: actor.ActionTaskGivenUp}},
		// "cleanup" has no events at all: never attempted.
	}

	snapshot := goal.CampaignSnapshot{
		GoalID: "listus",
		Statuses: map[string]goal.TaskStatus{
			"onboarding": goal.TaskCompleted,
			"add-items":  goal.TaskFailed,
			"checkout":   goal.TaskFailed,
			"cleanup":    goal.TaskPending,
		},
		Stopped: true, StopReason: goal.StopBudgetSteps,
	}

	callerFinding := campaign.Finding{
		Kind: campaign.FindingVerifiedDefect, TaskID: "checkout",
		Summary: "DTQL shows the order was never persisted despite a success message",
		Evidence: campaign.Evidence{
			ObservationSequences: []int64{4},
			LoopEventIndexes:     []int{5},
		},
		Confidence: "dtql-verified",
	}

	report := campaign.Assemble(campaign.AssembleInput{
		Goal: fourTaskGoal(), Campaign: snapshot, Events: events,
		CallerFindings: []campaign.Finding{callerFinding},
	})

	byTask := make(map[string][]campaign.Finding)
	for _, f := range report.Findings {
		byTask[f.TaskID] = append(byTask[f.TaskID], f)
	}

	if got := byTask["onboarding"]; len(got) != 0 {
		t.Fatalf("onboarding (completed) findings = %+v, want none", got)
	}

	navFailures := byTask["add-items"]
	if len(navFailures) != 1 || navFailures[0].Kind != campaign.FindingAINavigationFailure {
		t.Fatalf("add-items findings = %+v, want exactly one ai-navigation-failure", navFailures)
	}

	gaps := byTask["cleanup"]
	if len(gaps) != 1 || gaps[0].Kind != campaign.FindingCoverageGap {
		t.Fatalf("cleanup findings = %+v, want exactly one coverage-gap", gaps)
	}

	checkoutFindings := byTask["checkout"]
	// checkout failed cleanly (no stale/invalid history of its own) — the
	// mechanical rules must not invent an ai-navigation-failure or a
	// verified-defect for it; the only finding present is the one the
	// caller supplied, passed through untouched.
	if len(checkoutFindings) != 1 {
		t.Fatalf("checkout findings = %+v, want exactly the one caller-supplied finding", checkoutFindings)
	}
	if checkoutFindings[0].Kind != campaign.FindingVerifiedDefect || checkoutFindings[0].Confidence != "dtql-verified" {
		t.Fatalf("checkout finding = %+v, want the caller-supplied verified-defect passed through unchanged", checkoutFindings[0])
	}

	if len(report.Tasks) != 4 {
		t.Fatalf("report.Tasks has %d entries, want 4", len(report.Tasks))
	}
	for _, task := range report.Tasks {
		if task.TaskID == "cleanup" && task.Attempted {
			t.Fatal("cleanup.Attempted = true, want false (it has zero recorded events)")
		}
		if task.TaskID == "onboarding" && !task.Attempted {
			t.Fatal("onboarding.Attempted = false, want true")
		}
	}
}

// TestReportLinksEvidenceBySequence proves a finding's Evidence links back
// to the exact observation sequences and loop-event indexes that grounded
// it — the mechanism a developer uses to navigate from a claim to its proof
// — for both the mechanically derived ai-navigation-failure and an
// interrupted (coverage-gap) task, and that a never-attempted coverage-gap
// legitimately carries no evidence at all (there is nothing to link to).
func TestReportLinksEvidenceBySequence(t *testing.T) {
	events := []actor.LoopEvent{
		{Index: 0, TaskID: "add-items", ObservationSequence: 10, Action: actor.ActionOutcome{Kind: actor.ActionExecuted}},
		{Index: 1, TaskID: "add-items", ObservationSequence: 11,
			Validation: actor.ValidationOutcome{Checked: true, Verdict: observe.VerdictStale},
			Action:     actor.ActionOutcome{Kind: actor.ActionSkippedInvalid}},
		{Index: 2, TaskID: "add-items", ObservationSequence: 12,
			Action: actor.ActionOutcome{Kind: actor.ActionResolutionFailed}},
		{Index: 3, TaskID: "add-items", ObservationSequence: 13, Action: actor.ActionOutcome{Kind: actor.ActionTaskGivenUp}},

		{Index: 4, TaskID: "checkout", ObservationSequence: 20, Action: actor.ActionOutcome{Kind: actor.ActionExecuted}},
		// checkout is left Active: interrupted mid-task by a budget.
	}

	snapshot := goal.CampaignSnapshot{
		GoalID: "listus",
		Statuses: map[string]goal.TaskStatus{
			"onboarding": goal.TaskPending,
			"add-items":  goal.TaskFailed,
			"checkout":   goal.TaskActive,
			"cleanup":    goal.TaskPending,
		},
		Stopped: true, StopReason: goal.StopBudgetSteps,
	}

	report := campaign.Assemble(campaign.AssembleInput{Goal: fourTaskGoal(), Campaign: snapshot, Events: events})

	byTask := make(map[string]campaign.Finding)
	for _, f := range report.Findings {
		byTask[f.TaskID] = f
	}

	navFinding, ok := byTask["add-items"]
	if !ok || navFinding.Kind != campaign.FindingAINavigationFailure {
		t.Fatalf("add-items finding = %+v, %v, want an ai-navigation-failure", navFinding, ok)
	}
	// Both the stale-validation event (index 1) and the unresolvable-action
	// event (index 2) contributed evidence; the clean events (0, 3) did not.
	wantSeqs := map[int64]bool{11: true, 12: true}
	if len(navFinding.Evidence.ObservationSequences) != 2 {
		t.Fatalf("add-items evidence sequences = %v, want exactly [11, 12] (in some order)", navFinding.Evidence.ObservationSequences)
	}
	for _, seq := range navFinding.Evidence.ObservationSequences {
		if !wantSeqs[seq] {
			t.Fatalf("add-items evidence sequences = %v, want only 11 and 12", navFinding.Evidence.ObservationSequences)
		}
	}
	wantIdxs := map[int]bool{1: true, 2: true}
	for _, idx := range navFinding.Evidence.LoopEventIndexes {
		if !wantIdxs[idx] {
			t.Fatalf("add-items evidence loop-event indexes = %v, want only 1 and 2", navFinding.Evidence.LoopEventIndexes)
		}
		if events[idx].TaskID != "add-items" {
			t.Fatalf("evidence index %d points at event for task %q, not add-items", idx, events[idx].TaskID)
		}
	}

	interrupted, ok := byTask["checkout"]
	if !ok || interrupted.Kind != campaign.FindingCoverageGap {
		t.Fatalf("checkout finding = %+v, %v, want a coverage-gap (interrupted mid-task)", interrupted, ok)
	}
	if len(interrupted.Evidence.ObservationSequences) != 1 || interrupted.Evidence.ObservationSequences[0] != 20 {
		t.Fatalf("checkout evidence sequences = %v, want [20]", interrupted.Evidence.ObservationSequences)
	}
	if len(interrupted.Evidence.LoopEventIndexes) != 1 || interrupted.Evidence.LoopEventIndexes[0] != 4 {
		t.Fatalf("checkout evidence loop-event indexes = %v, want [4]", interrupted.Evidence.LoopEventIndexes)
	}

	neverAttempted, ok := byTask["cleanup"]
	if !ok || neverAttempted.Kind != campaign.FindingCoverageGap {
		t.Fatalf("cleanup finding = %+v, %v, want a coverage-gap", neverAttempted, ok)
	}
	if len(neverAttempted.Evidence.ObservationSequences) != 0 || len(neverAttempted.Evidence.LoopEventIndexes) != 0 {
		t.Fatalf("cleanup (never attempted) evidence = %+v, want empty — there is nothing to link to", neverAttempted.Evidence)
	}
}

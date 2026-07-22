package run

import (
	"time"

	"github.com/chatwright/chatwright/goal"
)

// RunCeiling optionally aggregates ai-goal usage across every Part in a Run,
// on top of each ai-goal Part's own goal.Budgets — hybrid-runs.md's "a
// run-level ceiling aggregates" across parts. Every field's zero value means
// "no limit", mirroring goal.Budgets' own convention, so the zero
// RunCeiling (Run's own default) means no run-level ceiling at all: every
// Part runs bounded only by its own goal.Budgets.
//
// Only ai-goal Parts contribute to a RunCeiling's steps/cost accounting —
// deterministic Parts have no goal.Budgets-shaped notion of "a step" or "a
// cost" to aggregate. MaxDuration is checked at the same points (between an
// ai-goal Part's tasks) rather than continuously against wall-clock time
// throughout the whole Run, including any deterministic Parts' own
// duration — a deliberate simplification: see ceilingTracker.record and
// Run.runAIGoal's own doc comment on why "between tasks" is this package's
// finest checkpoint.
type RunCeiling struct {
	// MaxSteps caps the total number of actor.LoopEvents recorded across
	// every ai-goal Part in the Run.
	MaxSteps int
	// MaxCost caps the total accrued actor.Usage.Cost across every ai-goal
	// Part in the Run. Nil means cost is not budgeted at the run level,
	// mirroring goal.Budgets.MaxCost.
	MaxCost *float64
	// MaxDuration caps wall-clock time elapsed (per Environment.Now) since
	// Run.Execute started, checked between an ai-goal Part's tasks.
	MaxDuration time.Duration
}

// CeilingTrip attributes a RunCeiling trip to both the aggregate reason and
// the Part that was executing when it fired — hybrid-runs.md's "when the
// ceiling trips mid-part, the stop reason must attribute both the run
// ceiling and the part it tripped in". Reason reuses goal.StopReason's own
// budget vocabulary (StopBudgetSteps, StopBudgetCost, StopBudgetDuration)
// rather than a parallel type, since a run-level ceiling trip is the same
// kind of budget exhaustion goal.CampaignState already models per Part,
// just aggregated across Parts.
type CeilingTrip struct {
	Reason goal.StopReason
	PartID string
}

// ceilingTracker accumulates ai-goal usage across a Run's Parts and reports
// a CeilingTrip the moment RunCeiling is exceeded.
type ceilingTracker struct {
	ceiling  RunCeiling
	now      func() time.Time
	runStart time.Time

	steps int
	cost  float64
}

// record adds one task's worth of newly recorded steps/cost to the
// tracker's running totals and reports a CeilingTrip, attributed to partID,
// the moment any dimension is exceeded. Checks run in a fixed, deterministic
// order — steps, then cost, then duration — mirroring
// goal.CampaignState.RecordStep's own steps-before-duration check order.
func (t *ceilingTracker) record(partID string, deltaSteps int, deltaCost float64) *CeilingTrip {
	t.steps += deltaSteps
	t.cost += deltaCost

	if max := t.ceiling.MaxSteps; max > 0 && t.steps >= max {
		return &CeilingTrip{Reason: goal.StopBudgetSteps, PartID: partID}
	}
	if max := t.ceiling.MaxCost; max != nil && *max > 0 && t.cost >= *max {
		return &CeilingTrip{Reason: goal.StopBudgetCost, PartID: partID}
	}
	if max := t.ceiling.MaxDuration; max > 0 && t.now().Sub(t.runStart) >= max {
		return &CeilingTrip{Reason: goal.StopBudgetDuration, PartID: partID}
	}
	return nil
}

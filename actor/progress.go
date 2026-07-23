package actor

import (
	"chatwright.dev/runtime/goal"
)

// ProgressPhase names when a ProgressSnapshot was emitted — see
// Config.OnProgress.
type ProgressPhase string

// Progress phases. See ProgressPhase.
const (
	// ProgressTaskStarted: RunTask just activated (or resumed) TaskID;
	// Iteration is 0, no LoopEvent for this task exists yet.
	ProgressTaskStarted ProgressPhase = "task-started"
	// ProgressIteration: one observe-plan-act-validate cycle just recorded
	// a LoopEvent for TaskID (Iteration counts it).
	ProgressIteration ProgressPhase = "iteration"
	// ProgressTaskEnded: RunTask is about to return for TaskID, for any
	// reason (terminal status, campaign stop, non-progress) — see
	// TaskResult.
	ProgressTaskEnded ProgressPhase = "task-ended"
)

// String renders p for diagnostics and formatted stage lines.
func (p ProgressPhase) String() string { return string(p) }

// BudgetBurn reports one goal.Budgets dimension's consumption as a
// fraction of its configured maximum: 0 when that dimension is unbudgeted
// (goal.Budgets' own "zero means unlimited" convention — an unbudgeted
// dimension is never "burned"), otherwise consumed/max, which is >= 1 the
// moment that dimension's own budget stop fires. RepeatedFailures is
// scoped to the CURRENT task only (goal.CampaignState.FailureCount is
// per-task), unlike the other three dimensions, which are campaign-wide.
type BudgetBurn struct {
	Steps            float64 `json:"steps"`
	Duration         float64 `json:"duration"`
	Cost             float64 `json:"cost"`
	RepeatedFailures float64 `json:"repeatedFailures"`
}

// ProgressSnapshot is one derived, point-in-time report of a Loop's
// progress through its current task — spec/ideas/campaign-progress-reporting.md's
// "three honest gauges" (goal progress, budget burn, health), assembled
// from state the loop already computes. Never persisted, never added to a
// run bundle: see Config.OnProgress.
type ProgressSnapshot struct {
	Phase ProgressPhase `json:"phase"`

	GoalID string `json:"goalId"`
	TaskID string `json:"taskId"`
	// TaskIndex is TaskID's 1-based position within the Goal's declared
	// Tasks — the idea's "task j/m" gauge (j).
	TaskIndex int `json:"taskIndex"`
	// TaskCount is the Goal's total declared task count — the idea's
	// "task j/m" gauge (m).
	TaskCount int `json:"taskCount"`
	// TasksCompleted is how many of the Goal's tasks are goal.TaskCompleted
	// as of this snapshot (campaign-wide, not just this task).
	TasksCompleted int `json:"tasksCompleted"`

	// Iteration is this task's own 1-based loop-iteration count: 0 at
	// ProgressTaskStarted, incrementing by one per ProgressIteration.
	Iteration int `json:"iteration"`

	Budgets goal.Budgets `json:"budgets"`
	Burn    BudgetBurn   `json:"burn"`

	// NonProgressStreak mirrors Loop.RunTask's own consecutive
	// invalid-or-no-effect counter for this task.
	NonProgressStreak int `json:"nonProgressStreak"`
	// RetryCounts tallies this task's own recorded LoopEvents so far, by
	// ActionOutcomeKind — the idea's "retry counts by cause" gauge.
	RetryCounts map[ActionOutcomeKind]int `json:"retryCounts"`

	// Stopped and StopReason mirror goal.CampaignState.Stopped/StopReason
	// as of this snapshot.
	Stopped    bool            `json:"stopped"`
	StopReason goal.StopReason `json:"stopReason"`
}

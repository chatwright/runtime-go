package goal

// TaskStatus is a task's position in its guarded lifecycle:
//
//	pending -> active -> completed | failed | blocked | skipped
//
// Only CampaignState mutates a task's status, and only along that guard: a
// task must be activated before it can reach any terminal status, and every
// terminal status is final.
type TaskStatus string

// Task lifecycle statuses. See TaskStatus.
const (
	TaskPending   TaskStatus = "pending"
	TaskActive    TaskStatus = "active"
	TaskCompleted TaskStatus = "completed"
	TaskFailed    TaskStatus = "failed"
	TaskBlocked   TaskStatus = "blocked"
	TaskSkipped   TaskStatus = "skipped"
)

// Terminal reports whether s is one of the lifecycle's terminal outcomes.
// Once a task reaches a terminal status no further transition is possible.
func (s TaskStatus) Terminal() bool {
	switch s {
	case TaskCompleted, TaskFailed, TaskBlocked, TaskSkipped:
		return true
	default:
		return false
	}
}

// StopReason is why a CampaignState stopped accepting further mutations.
// Every stop names exactly one reason, chosen deterministically by the
// condition that caused it.
type StopReason string

// Stop reasons. See CampaignState.StopReason.
const (
	// StopGoalComplete means every task reached a terminal status — there
	// is no more eligible work left to activate. It does not by itself mean
	// every task succeeded; read individual TaskStatus values for that.
	StopGoalComplete StopReason = "goal-complete"
	// StopBudgetSteps means Budgets.MaxSteps was reached.
	StopBudgetSteps StopReason = "budget-steps"
	// StopBudgetDuration means Budgets.MaxDuration elapsed.
	StopBudgetDuration StopReason = "budget-duration"
	// StopRepeatedFailure means Budgets.MaxRepeatedFailures was reached for
	// one task.
	StopRepeatedFailure StopReason = "repeated-failure"
	// StopBudgetCost means Budgets.MaxCost was reached via RecordCost.
	StopBudgetCost StopReason = "budget-cost"
	// StopCancelled means CampaignState.Cancel was called.
	StopCancelled StopReason = "cancelled"
	// StopError means CampaignState.Abort was called after an unrecoverable
	// runtime failure.
	StopError StopReason = "error"
	// StopGoalMetByEvidence means every task reached a terminal status, and
	// the transition that made the LAST one terminal was a
	// CampaignState.CompleteByEvidence call — the loop's own
	// machine-checkable criteria evaluation, not the actor's own task-done
	// claim, is what actually closed the campaign out. It is otherwise
	// exactly like StopGoalComplete (checkGoalComplete's own "every task is
	// terminal" condition): the distinct reason exists so a report can
	// name evidence, not the actor's own wrap-up, as what ended the run —
	// see CampaignState.CompleteByEvidence and
	// spec/ideas/evidence-defined-completion.md in the chatwright/chatwright
	// standard repository.
	StopGoalMetByEvidence StopReason = "goal-met-by-evidence"
)

package goal

import (
	"errors"
	"fmt"
)

// Goal.Validate errors.
var (
	// ErrEmptyTaskID means a Task's ID is empty or whitespace-only.
	ErrEmptyTaskID = errors.New("goal: task id is empty")
	// ErrDuplicateTaskID means two Tasks in the same Goal share an ID.
	ErrDuplicateTaskID = errors.New("goal: duplicate task id")
	// ErrUnknownDependency means a Task.DependsOn entry does not name a
	// Task ID present in the same Goal.
	ErrUnknownDependency = errors.New("goal: unknown dependency")
	// ErrDependencyCycle means the Task dependency graph contains a cycle.
	ErrDependencyCycle = errors.New("goal: dependency cycle")
	// ErrNegativeBudget means a Budgets field that must be zero (unlimited)
	// or positive was set negative.
	ErrNegativeBudget = errors.New("goal: budget must not be negative")
	// ErrNonPositiveCostBudget means Budgets.MaxCost was set to zero or a
	// negative value; leave it nil to mean "not budgeted".
	ErrNonPositiveCostBudget = errors.New("goal: max cost budget must be positive when set")
)

// CampaignState errors.
var (
	// ErrNilClock means NewCampaignState was called without a clock
	// function.
	ErrNilClock = errors.New("goal: clock function is nil")
	// ErrUnknownTask means a task id does not belong to the campaign's Goal.
	ErrUnknownTask = errors.New("goal: unknown task id")
	// ErrTaskNotEligible means Activate was called on a Pending task whose
	// DependsOn tasks are not all Completed.
	ErrTaskNotEligible = errors.New("goal: task is not eligible (unmet dependencies)")
	// ErrTaskNotActivatable means Activate was called on a task that was
	// not Pending — including an already-Active or already-terminal task.
	ErrTaskNotActivatable = errors.New("goal: task is not activatable")
	// ErrTaskNotActive means Complete, Fail, Block or Skip was called on a
	// task that was not currently Active.
	ErrTaskNotActive = errors.New("goal: task is not active")
	// ErrCampaignStopped means a mutating method was called after the
	// campaign had already stopped.
	ErrCampaignStopped = errors.New("goal: campaign has already stopped")
)

// fmtBudgetErr wraps a sentinel budget error with the offending field name
// and value.
func fmtBudgetErr(sentinel error, field string, value any) error {
	return fmt.Errorf("%w: %s = %v", sentinel, field, value)
}

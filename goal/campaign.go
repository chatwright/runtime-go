package goal

import (
	"fmt"
	"sync"
	"time"
)

// CampaignState is the guarded runtime state machine for one Goal: task
// statuses, elapsed steps and duration, per-task failure counts, and the
// deterministic StopReason that ends the campaign. It performs no AI,
// networking or platform I/O — callers report progress in with RecordStep,
// RecordFailure and the task transition methods, and read state back out.
//
// All methods are safe for concurrent use. Time comes from an injected
// clock (see NewCampaignState) rather than time.Now, so tests are
// deterministic and reproducible.
type CampaignState struct {
	goal  Goal
	tasks map[string]Task // by id, indexed once at construction
	now   func() time.Time

	mu         sync.Mutex
	statuses   map[string]TaskStatus
	failures   map[string]int
	steps      int
	startedAt  time.Time
	stopped    bool
	stopReason StopReason
	stoppedAt  time.Time
}

// NewCampaignState validates g (see Goal.Validate) and starts a new
// campaign with every task Pending. now supplies the current time for step
// duration and budget checks; pass a fixed or fake clock in tests so
// duration-budget behaviour is deterministic. now must not be nil.
func NewCampaignState(g Goal, now func() time.Time) (*CampaignState, error) {
	if now == nil {
		return nil, ErrNilClock
	}
	if err := g.Validate(); err != nil {
		return nil, err
	}

	tasks := make(map[string]Task, len(g.Tasks))
	statuses := make(map[string]TaskStatus, len(g.Tasks))
	for _, t := range g.Tasks {
		tasks[t.ID] = t
		statuses[t.ID] = TaskPending
	}

	return &CampaignState{
		goal:      g,
		tasks:     tasks,
		now:       now,
		statuses:  statuses,
		failures:  make(map[string]int, len(g.Tasks)),
		startedAt: now(),
	}, nil
}

// TaskStatus returns the current status of the task with the given id, or
// an error wrapping ErrUnknownTask if no such task exists.
func (c *CampaignState) TaskStatus(id string) (TaskStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	status, ok := c.statuses[id]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrUnknownTask, id)
	}
	return status, nil
}

// Eligible reports whether the task with the given id is currently Pending
// and every task it DependsOn is Completed — the guard Activate enforces.
// It errors if the task id is unknown.
func (c *CampaignState) Eligible(id string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	status, ok := c.statuses[id]
	if !ok {
		return false, fmt.Errorf("%w: %s", ErrUnknownTask, id)
	}
	return status == TaskPending && c.dependenciesComplete(id), nil
}

// dependenciesComplete reports whether every task the given task DependsOn
// is Completed. Caller must hold c.mu.
func (c *CampaignState) dependenciesComplete(id string) bool {
	for _, dep := range c.tasks[id].DependsOn {
		if c.statuses[dep] != TaskCompleted {
			return false
		}
	}
	return true
}

// Activate transitions a Pending, dependency-satisfied task to Active. It
// errors if:
//
//   - the campaign has already stopped (ErrCampaignStopped);
//   - the task id is unknown (ErrUnknownTask);
//   - the task is Pending but its dependencies are not all Completed
//     (ErrTaskNotEligible);
//   - the task is not Pending at all — including an already-Active or a
//     terminal task (ErrTaskNotActivatable).
func (c *CampaignState) Activate(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return c.stoppedErr()
	}
	status, ok := c.statuses[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownTask, id)
	}
	if status != TaskPending {
		return fmt.Errorf("%w: %s is %s", ErrTaskNotActivatable, id, status)
	}
	if !c.dependenciesComplete(id) {
		return fmt.Errorf("%w: %s", ErrTaskNotEligible, id)
	}
	c.statuses[id] = TaskActive
	return nil
}

// Complete transitions an Active task to Completed.
func (c *CampaignState) Complete(id string) error { return c.terminalize(id, TaskCompleted) }

// Fail transitions an Active task to Failed.
func (c *CampaignState) Fail(id string) error { return c.terminalize(id, TaskFailed) }

// Block transitions an Active task to Blocked.
func (c *CampaignState) Block(id string) error { return c.terminalize(id, TaskBlocked) }

// Skip transitions an Active task to Skipped.
func (c *CampaignState) Skip(id string) error { return c.terminalize(id, TaskSkipped) }

// terminalize moves the task with the given id from Active to target. It
// errors if the campaign has already stopped, the task id is unknown, or
// the task is not currently Active (ErrTaskNotActive) — a task must be
// activated before any terminal transition, and a terminal task cannot be
// re-terminalized. On success it checks whether the whole campaign has run
// out of eligible work and, if so, stops it with StopGoalComplete.
func (c *CampaignState) terminalize(id string, target TaskStatus) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return c.stoppedErr()
	}
	status, ok := c.statuses[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownTask, id)
	}
	if status != TaskActive {
		return fmt.Errorf("%w: %s is %s", ErrTaskNotActive, id, status)
	}
	c.statuses[id] = target
	c.checkGoalComplete()
	return nil
}

// checkGoalComplete stops the campaign with StopGoalComplete once every
// task has reached a terminal status — there is no more eligible work left
// to activate. It does not judge whether the outcome was a full success;
// callers read individual TaskStatus values for that. Caller must hold
// c.mu.
func (c *CampaignState) checkGoalComplete() {
	for _, status := range c.statuses {
		if !status.Terminal() {
			return
		}
	}
	c.stop(StopGoalComplete)
}

// RecordStep counts one action/step against the campaign's step and
// duration budgets. Call it once per recorded actor action or scenario
// step — never derive a step count from time.Now internally; this method's
// only notion of "now" is the injected clock. It stops the campaign
// deterministically (StopBudgetSteps, then StopBudgetDuration) the moment a
// positive budget is reached, and errors if the campaign has already
// stopped.
func (c *CampaignState) RecordStep() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return c.stoppedErr()
	}
	c.steps++
	if max := c.goal.Budgets.MaxSteps; max > 0 && c.steps >= max {
		c.stop(StopBudgetSteps)
		return nil
	}
	if max := c.goal.Budgets.MaxDuration; max > 0 && c.now().Sub(c.startedAt) >= max {
		c.stop(StopBudgetDuration)
	}
	return nil
}

// RecordFailure attributes one failed attempt to the task with the given
// id. Repeated failures against the same task accumulate across calls —
// the task does not need to be re-activated between them — and once the
// count reaches a positive Budgets.MaxRepeatedFailures the campaign stops
// with StopRepeatedFailure. It errors if the campaign has already stopped
// or the task id is unknown.
func (c *CampaignState) RecordFailure(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return c.stoppedErr()
	}
	if _, ok := c.statuses[id]; !ok {
		return fmt.Errorf("%w: %s", ErrUnknownTask, id)
	}
	c.failures[id]++
	if max := c.goal.Budgets.MaxRepeatedFailures; max > 0 && c.failures[id] >= max {
		c.stop(StopRepeatedFailure)
	}
	return nil
}

// Cancel stops the campaign with StopCancelled — an external decision to
// end the run early, distinct from any budget being exhausted. It errors if
// the campaign has already stopped.
func (c *CampaignState) Cancel() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return c.stoppedErr()
	}
	c.stop(StopCancelled)
	return nil
}

// Abort stops the campaign with StopError, after an unrecoverable runtime
// failure the caller cannot attribute to a budget or an explicit
// cancellation. It errors if the campaign has already stopped.
func (c *CampaignState) Abort() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return c.stoppedErr()
	}
	c.stop(StopError)
	return nil
}

// stop marks the campaign stopped with reason, unless it already stopped.
// Caller must hold c.mu.
func (c *CampaignState) stop(reason StopReason) {
	if c.stopped {
		return
	}
	c.stopped = true
	c.stopReason = reason
	c.stoppedAt = c.now()
}

// stoppedErr reports ErrCampaignStopped together with the reason the
// campaign stopped. Caller must hold c.mu.
func (c *CampaignState) stoppedErr() error {
	return fmt.Errorf("%w: reason=%s", ErrCampaignStopped, c.stopReason)
}

// Stopped reports whether the campaign has stopped accepting mutations.
func (c *CampaignState) Stopped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopped
}

// StopReason returns the reason the campaign stopped and true, or ("",
// false) while the campaign is still running.
func (c *CampaignState) StopReason() (StopReason, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.stopped {
		return "", false
	}
	return c.stopReason, true
}

// Steps returns the number of steps RecordStep has counted so far.
func (c *CampaignState) Steps() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.steps
}

// FailureCount returns how many failures RecordFailure has counted against
// the given task id so far (zero for an unknown id, rather than an error —
// callers that need to distinguish "no failures" from "unknown task" should
// check TaskStatus first).
func (c *CampaignState) FailureCount(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failures[id]
}

// CampaignSnapshot is a detached, point-in-time copy of a CampaignState's
// progress: safe to retain, log or compare after the originating
// CampaignState has moved on.
type CampaignSnapshot struct {
	GoalID     string
	Statuses   map[string]TaskStatus
	Steps      int
	Elapsed    time.Duration
	Failures   map[string]int
	Stopped    bool
	StopReason StopReason
}

// Snapshot returns a detached copy of the campaign's current progress.
func (c *CampaignState) Snapshot() CampaignSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	statuses := make(map[string]TaskStatus, len(c.statuses))
	for id, status := range c.statuses {
		statuses[id] = status
	}
	failures := make(map[string]int, len(c.failures))
	for id, n := range c.failures {
		failures[id] = n
	}

	elapsedAt := c.stoppedAt
	if !c.stopped {
		elapsedAt = c.now()
	}

	return CampaignSnapshot{
		GoalID:     c.goal.ID,
		Statuses:   statuses,
		Steps:      c.steps,
		Elapsed:    elapsedAt.Sub(c.startedAt),
		Failures:   failures,
		Stopped:    c.stopped,
		StopReason: c.stopReason,
	}
}

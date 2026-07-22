package actor

import (
	"time"

	"github.com/chatwright/chatwright/observe"
)

// LoopEvent is one loop iteration's complete structured record: what was
// observed, what was proposed, how the proposal validated, what happened
// when the loop acted on it (or chose not to), and what it cost. LoopEvents
// are the loop's entire raw material for campaign.Report — nothing the
// report needs is reconstructed after the fact from logs or a transcript.
type LoopEvent struct {
	// Index is 0-based and monotonic across one Loop's lifetime (not just
	// one task), so it is stable to reference from a campaign.Finding.
	Index int
	// At is stamped from the loop's injected clock (Config.Now), never
	// time.Now, so a run's timeline is reproducible.
	At time.Time
	// TaskID is the task this iteration was attempting.
	TaskID string

	// ObservationSequence is the observe.Observation.Sequence this
	// iteration observed before proposing — the same value a
	// campaign.Finding's evidence links back to.
	ObservationSequence int64

	Proposal Proposal
	Usage    Usage

	// Validation is the loop's validate-step outcome for Proposal. It is
	// only Checked for ProposeClick — the loop has nothing to validate
	// against observe for a send-text, task-done or give-up proposal.
	Validation ValidationOutcome

	// Action is what actually happened when the loop tried to act on
	// Proposal (or why it did not).
	Action ActionOutcome
}

// ValidationOutcome is the loop's validate-step verdict for one proposal,
// carrying observe.Engine.Validate's own result verbatim when it applies.
type ValidationOutcome struct {
	// Checked is false for proposal kinds observe.Validate does not apply
	// to (ProposeSendText, ProposeTaskDone, ProposeGiveUp); Verdict and
	// Reason are meaningless when Checked is false.
	Checked bool
	Verdict observe.Verdict
	Reason  string
}

// ActionOutcomeKind classifies what happened when the loop acted on a
// proposal, or why it did not act at all.
type ActionOutcomeKind int

// Action outcome kinds. See ActionOutcome.
const (
	// ActionSkippedInvalid: the proposal failed validation (a stale click)
	// or was malformed; the loop never submitted anything to the platform.
	ActionSkippedInvalid ActionOutcomeKind = iota
	// ActionExecuted: the proposed action was submitted to the platform and
	// produced an observable change (a new message, an edit, or an
	// actions-changed update).
	ActionExecuted
	// ActionExecutedNoEffect: the proposed action was submitted, but the
	// next observation showed no change at all.
	ActionExecutedNoEffect
	// ActionResolutionFailed: a freshly validated proposal that the loop
	// could not resolve to a concrete platform action — e.g. no button on
	// the current message carries the validated action's label (see Loop's
	// single-live-surface scoping note). This counts as a task failure
	// (goal.CampaignState.RecordFailure).
	ActionResolutionFailed
	// ActionTaskCompleted: a ProposeTaskDone proposal was accepted;
	// goal.CampaignState.Complete was called for the task.
	ActionTaskCompleted
	// ActionTaskGivenUp: a ProposeGiveUp proposal was accepted;
	// goal.CampaignState.Fail was called for the task.
	ActionTaskGivenUp
)

// String renders k for diagnostics, test failure messages and reports.
func (k ActionOutcomeKind) String() string {
	switch k {
	case ActionSkippedInvalid:
		return "skipped-invalid"
	case ActionExecuted:
		return "executed"
	case ActionExecutedNoEffect:
		return "executed-no-effect"
	case ActionResolutionFailed:
		return "resolution-failed"
	case ActionTaskCompleted:
		return "task-completed"
	case ActionTaskGivenUp:
		return "task-given-up"
	default:
		return "unknown-action-outcome"
	}
}

// ActionOutcome is what actually happened when the loop tried to act on a
// Proposal.
type ActionOutcome struct {
	Kind ActionOutcomeKind
	// Detail is a human-readable explanation, set for
	// ActionSkippedInvalid/ActionResolutionFailed (why), empty otherwise.
	Detail string
}

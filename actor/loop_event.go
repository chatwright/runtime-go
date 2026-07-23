package actor

import (
	"time"

	"chatwright.dev/runtime/observe"
)

// LoopEvent is one loop iteration's complete structured record: what was
// observed, what was proposed, how the proposal validated, what happened
// when the loop acted on it (or chose not to), and what it cost. LoopEvents
// are the loop's entire raw material for campaign.Report — nothing the
// report needs is reconstructed after the fact from logs or a transcript.
type LoopEvent struct {
	// Index is 0-based and monotonic across one Loop's lifetime (not just
	// one task), so it is stable to reference from a campaign.Finding.
	Index int `json:"index"`
	// At is stamped from the loop's injected clock (Config.Now), never
	// time.Now, so a run's timeline is reproducible.
	At time.Time `json:"at"`
	// TaskID is the task this iteration was attempting.
	TaskID string `json:"taskId"`

	// ObservationSequence is the observe.Observation.Sequence this
	// iteration observed before proposing — the same value a
	// campaign.Finding's evidence links back to.
	ObservationSequence int64 `json:"observationSequence"`

	Proposal Proposal `json:"proposal"`
	Usage    Usage    `json:"usage"`

	// Validation is the loop's validate-step outcome for Proposal. It is
	// only Checked for ProposeClick — the loop has nothing to validate
	// against observe for a send-text, task-done or give-up proposal.
	Validation ValidationOutcome `json:"validation"`

	// Action is what actually happened when the loop tried to act on
	// Proposal (or why it did not).
	Action ActionOutcome `json:"action"`

	// ProposeError is set exactly when this iteration's call to
	// Provider.Propose returned an error: it carries that error's own
	// message (error.Error()), and Proposal, Usage, Validation and Action
	// are all their zero value — there was nothing to validate or act on.
	// Empty for every iteration that got as far as a Proposal.
	//
	// This field exists so a failed Propose call still leaves a LoopEvent
	// behind — see RunTask, which appends one before returning the error —
	// instead of vanishing from the record with only a returned Go error
	// nobody downstream of the loop (a campaign.Report, a run bundle) ever
	// sees (github.com/chatwright/runtime-go issue #4).
	ProposeError string `json:"proposeError,omitempty"`
}

// ValidationOutcome is the loop's validate-step verdict for one proposal,
// carrying observe.Engine.Validate's own result verbatim when it applies.
type ValidationOutcome struct {
	// Checked is false for proposal kinds observe.Validate does not apply
	// to (ProposeSendText, ProposeTaskDone, ProposeGiveUp); Verdict and
	// Reason are meaningless when Checked is false.
	Checked bool            `json:"checked"`
	Verdict observe.Verdict `json:"verdict"`
	Reason  string          `json:"reason"`
}

// ActionOutcomeKind classifies what happened when the loop acted on a
// proposal, or why it did not act at all. It is a string type, not an int
// enum, so it marshals to human-readable JSON (see AGENTS.md's "JSON
// artefacts carry human-readable string constants" convention) rather than a
// bare, meaningless integer.
type ActionOutcomeKind string

// Action outcome kinds. See ActionOutcome.
const (
	// ActionSkippedInvalid: the proposal failed validation (a stale click)
	// or was malformed; the loop never submitted anything to the platform.
	ActionSkippedInvalid ActionOutcomeKind = "skipped-invalid"
	// ActionExecuted: the proposed action was submitted to the platform and
	// produced a semantically observable change: a new message, or an
	// existing message whose text or action labels actually differ from
	// before (see observedEffect/semanticallyEqualMessage in loop.go).
	ActionExecuted ActionOutcomeKind = "executed"
	// ActionExecutedNoEffect: the proposed action was submitted, but the
	// next observation showed no semantic change — either genuinely no
	// observe.Change at all, or the only bot-authored Changes were
	// content-identical re-renders (e.g. a message re-edited in place with
	// byte-identical text and the same actions, which still bumps Version
	// and so still appears as an observe.Change — see
	// observedEffect/semanticallyEqualMessage in loop.go). This is
	// deliberately the same outcome kind either way: from a Provider's or a
	// report's point of view, "the platform re-showed exactly what was
	// already there" is not progress, regardless of whether observe's own
	// Version bookkeeping ticked over. It is what feeds NonProgressLimit.
	ActionExecutedNoEffect ActionOutcomeKind = "executed-no-effect"
	// ActionResolutionFailed: a freshly validated proposal that the loop
	// could not resolve to a concrete platform action — e.g. no button on
	// the current message carries the validated action's label (see Loop's
	// single-live-surface scoping note). This counts as a task failure
	// (goal.CampaignState.RecordFailure).
	ActionResolutionFailed ActionOutcomeKind = "resolution-failed"
	// ActionTaskCompleted: a ProposeTaskDone proposal was accepted;
	// goal.CampaignState.Complete was called for the task.
	ActionTaskCompleted ActionOutcomeKind = "task-completed"
	// ActionTaskGivenUp: a ProposeGiveUp proposal was accepted;
	// goal.CampaignState.Fail was called for the task.
	ActionTaskGivenUp ActionOutcomeKind = "task-given-up"
	// ActionBlockedConstraintViolation: a ProposeSendText proposal's text
	// violated the active task's (or goal's) machine-checkable content
	// rules (goal.EffectiveContentRules) — a vocabulary allowlist, a
	// deny-pattern or a custom predicate. The loop never submitted it to
	// the platform; see campaign.FindingConstraintViolation and
	// spec/ideas/proposal-content-constraints.md.
	ActionBlockedConstraintViolation ActionOutcomeKind = "blocked-constraint-violation"
	// ActionOvershootProbe: a proposal Loop.probeOvershoot requested and
	// recorded strictly to measure whether the actor would keep acting
	// after its task's goal.Task.Criteria already held. The loop never
	// submitted it to the platform; see campaign.FindingActorOvershoot and
	// spec/ideas/evidence-defined-completion.md.
	ActionOvershootProbe ActionOutcomeKind = "overshoot-probe"
)

// String renders k for diagnostics, test failure messages and reports.
func (k ActionOutcomeKind) String() string { return string(k) }

// ActionOutcome is what actually happened when the loop tried to act on a
// Proposal.
type ActionOutcome struct {
	Kind ActionOutcomeKind `json:"kind"`
	// Detail is a human-readable explanation, set for
	// ActionSkippedInvalid/ActionResolutionFailed (why), empty otherwise.
	Detail string `json:"detail"`
}

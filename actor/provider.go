// Package actor is Chatwright's AI actor loop: the observe-plan-act-validate
// cycle that drives a goal.CampaignState through a conversation using a
// pluggable Provider.
//
// The seam to any concrete model or vendor is exactly one interface —
// Provider — kept deliberately dumb: it reads a Prompt (goal/task context,
// the current observe.Observation, bounded recent history) and returns a
// Proposal. Every safety property lives in the loop, never in a Provider:
// budgets and stop reasons come from goal.CampaignState, click proposals are
// checked with observe.Engine.Validate, and Chatwright remains authoritative
// — an invalid proposal is recorded and re-prompted, never silently acted
// on. See Loop.
package actor

import (
	"context"
	"time"

	"chatwright.dev/runtime/observe"
)

// Provider proposes the next action for an in-flight campaign task. It is a
// dumb transport: read a Prompt, return a Proposal and the Usage it cost.
// Nothing a Provider returns is trusted blindly — see Loop for the
// validate-then-act guard every Proposal passes through.
type Provider interface {
	Propose(ctx context.Context, prompt Prompt) (Proposal, Usage, error)
}

// ProviderFunc adapts a plain function to the Provider interface, the same
// way http.HandlerFunc adapts a function to http.Handler.
type ProviderFunc func(ctx context.Context, prompt Prompt) (Proposal, Usage, error)

// Propose calls f.
func (f ProviderFunc) Propose(ctx context.Context, prompt Prompt) (Proposal, Usage, error) {
	return f(ctx, prompt)
}

// Prompt is everything a Provider needs to propose the next action: the
// goal/active-task context, the current semantic Observation — never raw
// platform payloads, see observe's own doctrine — and bounded recent
// history.
type Prompt struct {
	GoalID          string
	GoalTitle       string
	GoalDescription string
	Constraints     []string

	TaskID              string
	TaskTitle           string
	TaskSuccessCriteria string

	// Observation is the current semantic snapshot: visible messages,
	// available actions and explicit changes. A Provider proposing
	// ProposeClick must copy ActionID from an action listed here, and
	// ObservationSequence from Observation.Sequence.
	Observation observe.Observation

	// History is the loop's last N LoopEvents preceding this prompt, oldest
	// first; N is the loop's configured history window (Config.HistoryWindow).
	// It includes invalid/no-effect attempts, so a Provider can see (and
	// avoid repeating) what did not work.
	History []LoopEvent
}

// ProposalKind is the typed shape of a Provider's proposed action. It is a
// string type, not an int enum, so it marshals to human-readable JSON — in
// cassette files and everywhere else — rather than a bare, meaningless
// integer (see AGENTS.md's "JSON artefacts carry human-readable string
// constants" convention).
type ProposalKind string

// Proposal kinds. See Proposal.
const (
	// ProposeSendText: send free text as the user.
	ProposeSendText ProposalKind = "send-text"
	// ProposeClick: activate a previously observed AvailableAction by its
	// opaque ID (Proposal.ActionID), as seen at Proposal.ObservationSequence.
	ProposeClick ProposalKind = "click"
	// ProposeTaskDone: the active task's success criteria are met.
	ProposeTaskDone ProposalKind = "task-done"
	// ProposeGiveUp: the active task cannot be completed; stop attempting it.
	ProposeGiveUp ProposalKind = "give-up"
)

// String renders k for diagnostics, test failure messages and cassette
// files.
func (k ProposalKind) String() string { return string(k) }

// Proposal is a Provider's typed intent for the next action, plus its
// free-text rationale. The loop validates and executes it — see Loop.
type Proposal struct {
	Kind ProposalKind `json:"kind"`

	// Text is set for ProposeSendText: the text to send as the user.
	Text string `json:"text"`

	// ActionID is set for ProposeClick: an observe.AvailableAction.ID drawn
	// from the Prompt's Observation.
	ActionID string `json:"actionId"`
	// ObservationSequence is the Observation.Sequence the proposal was
	// chosen from. Required for ProposeClick (fed to observe.Engine.Validate
	// as observe.ActionProposal.ObservationSequence); ignored otherwise.
	ObservationSequence int64 `json:"observationSequence"`

	// Rationale is free text explaining the choice — never private
	// chain-of-thought, just enough for a developer or the campaign report
	// to understand why the actor did this.
	Rationale string `json:"rationale"`
}

// Usage reports what one Propose call cost: model identity, token counts,
// latency and, optionally, a caller-priced Cost. When Cost is set, the loop
// feeds it to goal.CampaignState.RecordCost so a configured
// goal.Budgets.MaxCost is enforced.
type Usage struct {
	Model        string        `json:"model"`
	InputTokens  int           `json:"inputTokens"`
	OutputTokens int           `json:"outputTokens"`
	Latency      time.Duration `json:"latencyNanoseconds"`
	Cost         *float64      `json:"cost,omitempty"`
}

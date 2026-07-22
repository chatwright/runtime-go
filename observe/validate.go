package observe

import "fmt"

// ActionProposal is an actor's intent to activate a previously observed
// action: the Observation it was chosen from, and the action's stable ID.
// Validate checks it against the Engine's CURRENT journal state — the actor
// proposes intent, it never makes that intent authoritative by asserting it
// (see spec/features/chatwright/observation-model/actor-actions).
type ActionProposal struct {
	ObservationSequence int64
	ActionID            string
}

// Verdict is the deterministic outcome of validating an ActionProposal.
type Verdict int

const (
	// VerdictFresh: the proposed action is present, unchanged, in the
	// Engine's current projection.
	VerdictFresh Verdict = iota
	// VerdictStale: the proposed action is not present in the Engine's
	// current projection — its source observation is out of date, or was
	// never issued by this Engine at all.
	VerdictStale
)

// String renders v for diagnostics and test failure messages.
func (v Verdict) String() string {
	if v == VerdictFresh {
		return "fresh"
	}
	return "stale"
}

// ValidationResult is the deterministic result of validating an
// ActionProposal.
type ValidationResult struct {
	Verdict Verdict
	// Reason explains the verdict; always set, safe to surface to a scripted
	// actor's assertion, an AI actor's recovery prompt, or Studio.
	Reason string
	// Current is the action's current form; set only when Verdict is
	// VerdictFresh.
	Current *AvailableAction
}

// Validate checks proposal against the Engine's CURRENT journal state —
// never against the (possibly outdated) Observation the actor originally
// saw — and returns a deterministic fresh/stale verdict with a reason.
// Validate does not execute anything, and it does not itself issue or count
// as a new Observation.
func (e *Engine) Validate(proposal ActionProposal) (ValidationResult, error) {
	e.mu.Lock()
	_, known := e.issued[proposal.ObservationSequence]
	latestSeq := e.seq
	e.mu.Unlock()

	if !known {
		return ValidationResult{
			Verdict: VerdictStale,
			Reason:  fmt.Sprintf("observation %d is unknown to this engine", proposal.ObservationSequence),
		}, nil
	}

	entries, err := e.journaler.Journal(e.chat.ChatID)
	if err != nil {
		return ValidationResult{}, fmt.Errorf("observe: validate: %w", err)
	}
	current := projectMessages(entries, latestSeq)

	for _, m := range current {
		for i := range m.Actions {
			if m.Actions[i].ID == proposal.ActionID {
				action := m.Actions[i]
				return ValidationResult{
					Verdict: VerdictFresh,
					Reason:  "action is currently available",
					Current: &action,
				}, nil
			}
		}
	}

	return ValidationResult{
		Verdict: VerdictStale,
		Reason:  fmt.Sprintf("action %q is no longer available (its message was edited or its actions changed)", proposal.ActionID),
	}, nil
}

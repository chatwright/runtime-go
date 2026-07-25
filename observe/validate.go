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

// Freshness is the deterministic outcome of validating an ActionProposal: is
// the proposed action still present, unchanged, in the Engine's current
// projection? It is a validity check against the Engine's own state, not a
// judgement against a criterion — see the chatwright/chatwright glossary's
// "verdict" entry for that (the AI-judged assertion outcome). It is a string
// type, not an int enum, so it marshals to human-readable JSON (see
// AGENTS.md's "JSON artefacts carry human-readable string constants"
// convention) rather than a bare, meaningless integer.
type Freshness string

const (
	// FreshnessFresh: the proposed action is present, unchanged, in the
	// Engine's current projection.
	FreshnessFresh Freshness = "fresh"
	// FreshnessStale: the proposed action is not present in the Engine's
	// current projection — its source observation is out of date, or was
	// never issued by this Engine at all.
	FreshnessStale Freshness = "stale"
)

// String renders f for diagnostics and test failure messages.
func (f Freshness) String() string { return string(f) }

// ValidationResult is the deterministic result of validating an
// ActionProposal.
type ValidationResult struct {
	Freshness Freshness
	// Reason explains Freshness; always set, safe to surface to a scripted
	// actor's assertion, an AI actor's recovery prompt, or Studio.
	Reason string
	// Current is the action's current form; set only when Freshness is
	// FreshnessFresh.
	Current *AvailableAction
}

// Validate checks proposal against the Engine's CURRENT journal state —
// never against the (possibly outdated) Observation the actor originally
// saw — and returns a deterministic fresh/stale Freshness with a reason.
// Validate does not execute anything, and it does not itself issue or count
// as a new Observation.
func (e *Engine) Validate(proposal ActionProposal) (ValidationResult, error) {
	e.mu.Lock()
	_, known := e.issued[proposal.ObservationSequence]
	latestSeq := e.seq
	e.mu.Unlock()

	if !known {
		return ValidationResult{
			Freshness: FreshnessStale,
			Reason:    fmt.Sprintf("observation %d is unknown to this engine", proposal.ObservationSequence),
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
					Freshness: FreshnessFresh,
					Reason:    "action is currently available",
					Current:   &action,
				}, nil
			}
		}
	}

	return ValidationResult{
		Freshness: FreshnessStale,
		Reason:    fmt.Sprintf("action %q is no longer available (its message was edited or its actions changed)", proposal.ActionID),
	}, nil
}

package goal

import (
	"context"
	"regexp"
	"strings"
)

// ContentPredicate is a custom, deterministic content check beyond
// ContentRules' own Vocabulary/DenyPatterns — the "predicate seam for
// custom rules" spec/ideas/proposal-content-constraints.md calls for. ok
// reports whether text is allowed; reason is a human-readable explanation
// used only when ok is false. A non-nil error means the check itself
// failed (not that text violated it) and is surfaced to the loop's caller,
// mirroring Criteria's own error convention.
type ContentPredicate func(ctx context.Context, text string) (ok bool, reason string, err error)

// ContentRules declares machine-checkable content rules for a Task's (or a
// Goal's) ProposeSendText proposals — the "missing symmetric half of
// evidence-defined completion" spec/ideas/proposal-content-constraints.md
// describes: Criteria judges the world after an action; ContentRules
// judges what the actor may say on the way in. All three dimensions are
// deterministic — no semantic/NLP judgement, per the idea's explicit "Not
// Doing".
//
// The zero value means "no rule": every text proposal passes. See
// EffectiveContentRules for how a Task's own ContentRules (when non-empty)
// overrides its Goal's, per the idea's "task overriding goal" resolution
// of its own open question — documented there, not repeated per call site.
type ContentRules struct {
	// Vocabulary is a case-insensitive allowlist of terms: text.Check fails,
	// citing this reason, when text (lower-cased) contains none of them as
	// a substring. Empty means no vocabulary check. Exact substring
	// matching only, deliberately: the idea's "Not Doing" rules out
	// semantic matching in this deterministic layer — a test author who
	// wants word-boundary precision supplies a DenyPatterns/Predicate
	// entry instead.
	Vocabulary []string

	// DenyPatterns are regular expressions checked against the proposal's
	// raw (not lower-cased) text; any match blocks it, regardless of
	// Vocabulary. Checked before Vocabulary, so a denied pattern is always
	// the reported reason even when the text also happens to contain an
	// allowed term.
	DenyPatterns []*regexp.Regexp

	// Predicate is an optional custom deterministic check, run last (after
	// DenyPatterns and Vocabulary both pass). Nil means no predicate
	// check.
	Predicate ContentPredicate
}

// Empty reports whether r declares no rule at all — the condition
// EffectiveContentRules uses to decide whether a Task's own ContentRules
// overrides its Goal's.
func (r ContentRules) Empty() bool {
	return len(r.Vocabulary) == 0 && len(r.DenyPatterns) == 0 && r.Predicate == nil
}

// Check judges text against r's rules, in the fixed order documented on
// each field (DenyPatterns, then Vocabulary, then Predicate — the first
// violation found is the one reported). ok is true, with an empty reason,
// when r is Empty() or text violates none of its rules. A non-nil error
// means Predicate itself failed, never that text violated a rule.
func (r ContentRules) Check(ctx context.Context, text string) (ok bool, reason string, err error) {
	for _, pattern := range r.DenyPatterns {
		if pattern == nil {
			continue
		}
		if pattern.MatchString(text) {
			return false, "text matches a denied pattern: " + pattern.String(), nil
		}
	}

	if len(r.Vocabulary) > 0 {
		lower := strings.ToLower(text)
		matched := false
		for _, term := range r.Vocabulary {
			if term == "" {
				continue
			}
			if strings.Contains(lower, strings.ToLower(term)) {
				matched = true
				break
			}
		}
		if !matched {
			return false, "text does not contain any allowed vocabulary term", nil
		}
	}

	if r.Predicate != nil {
		pOK, pReason, pErr := r.Predicate(ctx, text)
		if pErr != nil {
			return false, "", pErr
		}
		if !pOK {
			return false, pReason, nil
		}
	}

	return true, "", nil
}

// EffectiveContentRules resolves which ContentRules apply to t within g:
// t.ContentRules when it is non-Empty(), otherwise g.ContentRules — "task
// overriding goal" (spec/ideas/proposal-content-constraints.md's own open
// question, resolved this way and documented here as its single source of
// truth). A Task never merges its rules with its Goal's; declaring any
// task-level rule takes the goal-level rule out of the picture entirely
// for that task.
func EffectiveContentRules(g Goal, t Task) ContentRules {
	if !t.ContentRules.Empty() {
		return t.ContentRules
	}
	return g.ContentRules
}

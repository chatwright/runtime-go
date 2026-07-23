package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"chatwright.dev/runtime/actor"
)

// wireProposal is the JSON shape responseJSONSchema (prompt.go) describes —
// the SAME shape actor/anthropic's own wireProposal enforces. Kind is a
// plain string, not actor.ProposalKind, deliberately: an absent "kind" key
// would silently decode to actor.ProposalKind's zero value (ProposeSendText)
// rather than surface as an error, and toProposal below needs to tell
// "missing" apart from "send-text" to avoid fabricating a proposal.
type wireProposal struct {
	Kind      string `json:"kind"`
	Text      string `json:"text"`
	ActionID  string `json:"action_id"`
	Rationale string `json:"rationale"`
}

// proposalFromResponse extracts resp's first choice's message content,
// parses it as a wireProposal (with one repair attempt — see
// parseWireProposal), and converts it to an actor.Proposal. It never
// returns a non-error actor.Proposal built from an unparseable or
// contract-violating reply: every failure path returns
// *InvalidResponseError instead.
func proposalFromResponse(resp *chatCompletionResponse, prompt actor.Prompt) (actor.Proposal, error) {
	raw, finishReason, err := responseText(resp)
	if err != nil {
		return actor.Proposal{}, &InvalidResponseError{FinishReason: finishReason, Err: err}
	}

	wp, err := parseWireProposal(raw)
	if err != nil {
		return actor.Proposal{}, &InvalidResponseError{Raw: raw, FinishReason: finishReason, Err: err}
	}

	proposal, err := wp.toProposal(prompt)
	if err != nil {
		return actor.Proposal{}, &InvalidResponseError{Raw: raw, FinishReason: finishReason, Err: err}
	}
	return proposal, nil
}

// responseText returns resp's first choice's message content, and that
// choice's finish_reason. A response can carry no usable content at all —
// no choices, or a choice whose message content is empty (e.g. a content
// filter refusal, or a reply cut off by finish_reason "length" before any
// text landed) — either way there is nothing to parse.
func responseText(resp *chatCompletionResponse) (raw, finishReason string, err error) {
	if len(resp.Choices) == 0 {
		return "", "", errors.New("response has no choices")
	}
	choice := resp.Choices[0]
	if choice.Message.Content == "" {
		return "", choice.FinishReason, fmt.Errorf("response's first choice has empty content (finish_reason=%s)", choice.FinishReason)
	}
	return choice.Message.Content, choice.FinishReason, nil
}

// parseWireProposal parses raw as a wireProposal, with one repair attempt:
// if raw does not parse as-is (the model wrapped the JSON object in prose
// or a markdown fence despite the response contract saying not to — more
// likely in ModeJSONObjectFallback, where there is no server-side schema
// enforcement, but attempted uniformly regardless of mode), it retries once
// against the substring from raw's first '{' to its last '}'. A second
// failure is returned verbatim — this package makes exactly one repair
// attempt, never more, and never fabricates a Proposal in place of a
// successful parse.
func parseWireProposal(raw string) (wireProposal, error) {
	var wp wireProposal
	if err := json.Unmarshal([]byte(raw), &wp); err == nil {
		return wp, nil
	}

	repaired, ok := extractJSONObject(raw)
	if !ok {
		return wireProposal{}, errors.New("no JSON object found in response text")
	}
	if err := json.Unmarshal([]byte(repaired), &wp); err != nil {
		return wireProposal{}, fmt.Errorf("unparseable even after one repair attempt: %w", err)
	}
	return wp, nil
}

// extractJSONObject returns the substring of s from its first '{' to its
// last '}', inclusive — a deliberately simple repair heuristic. It does not
// attempt to fix truncated or otherwise malformed JSON inside that span;
// parseWireProposal's second json.Unmarshal is the actual validity check.
func extractJSONObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end < start {
		return "", false
	}
	return s[start : end+1], true
}

// toProposal converts wp to an actor.Proposal, checking that the fields its
// Kind requires are actually set. It does not check whether a "click"
// ActionID is still valid — that stale-action check is the loop's job
// (observe.Engine.Validate against the engine's CURRENT state), never a
// Provider's; see actor's package doc. ObservationSequence is never taken
// from the model's reply at all: for a "click" proposal it is always
// prompt.Observation.Sequence, the only observation the model could have
// seen this turn.
func (wp wireProposal) toProposal(prompt actor.Prompt) (actor.Proposal, error) {
	if wp.Rationale == "" {
		return actor.Proposal{}, errors.New(`"rationale" is empty`)
	}

	kind, err := parseProposalKind(wp.Kind)
	if err != nil {
		return actor.Proposal{}, err
	}

	p := actor.Proposal{Kind: kind, Rationale: wp.Rationale}
	switch kind {
	case actor.ProposeSendText:
		if wp.Text == "" {
			return actor.Proposal{}, errors.New(`kind "send-text" requires non-empty "text"`)
		}
		p.Text = wp.Text
	case actor.ProposeClick:
		if wp.ActionID == "" {
			return actor.Proposal{}, errors.New(`kind "click" requires non-empty "action_id"`)
		}
		p.ActionID = wp.ActionID
		p.ObservationSequence = prompt.Observation.Sequence
	case actor.ProposeTaskDone, actor.ProposeGiveUp:
		// No further fields required.
	}
	return p, nil
}

// parseProposalKind maps s to an actor.ProposalKind by comparing it against
// each kind's own String() form — the single source of truth for the wire
// vocabulary — rejecting both an empty/missing "kind" and an unrecognised
// one, rather than silently defaulting to send-text (actor.ProposalKind's
// zero value).
func parseProposalKind(s string) (actor.ProposalKind, error) {
	if s == "" {
		return "", errors.New(`"kind" is missing`)
	}
	for _, k := range []actor.ProposalKind{actor.ProposeSendText, actor.ProposeClick, actor.ProposeTaskDone, actor.ProposeGiveUp} {
		if k.String() == s {
			return k, nil
		}
	}
	return "", fmt.Errorf("unknown proposal kind %q", s)
}

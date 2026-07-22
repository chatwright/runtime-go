package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/chatwright/chatwright/actor"
)

// wireProposal is the JSON shape responseJSONSchema (prompt.go) describes.
// Kind is a plain string, not actor.ProposalKind, deliberately: an absent
// "kind" key would silently decode to actor.ProposalKind's zero value
// (ProposeSendText) rather than surface as an error, and toProposal below
// needs to tell "missing" apart from "send-text" to avoid fabricating a
// proposal. The wire vocabulary is still tied to actor.ProposalKind's own
// String() forms by construction — see toProposal.
type wireProposal struct {
	Kind      string `json:"kind"`
	Text      string `json:"text"`
	ActionID  string `json:"action_id"`
	Rationale string `json:"rationale"`
}

// proposalFromResponse extracts resp's text content, parses it as a
// wireProposal (with one repair attempt — see parseWireProposal), and
// converts it to an actor.Proposal. It never returns a non-error
// actor.Proposal built from an unparseable or contract-violating reply:
// every failure path returns *InvalidResponseError instead.
func proposalFromResponse(resp *sdk.Message, prompt actor.Prompt) (actor.Proposal, error) {
	raw, err := responseText(resp)
	if err != nil {
		return actor.Proposal{}, &InvalidResponseError{StopReason: string(resp.StopReason), Err: err}
	}

	wp, err := parseWireProposal(raw)
	if err != nil {
		return actor.Proposal{}, &InvalidResponseError{Raw: raw, StopReason: string(resp.StopReason), Err: err}
	}

	proposal, err := wp.toProposal(prompt)
	if err != nil {
		return actor.Proposal{}, &InvalidResponseError{Raw: raw, StopReason: string(resp.StopReason), Err: err}
	}
	return proposal, nil
}

// responseText returns the text of resp's first non-empty text content
// block. An API response can carry no text at all — e.g. a safety-classifier
// refusal (stop_reason "refusal") can return an empty content array, and a
// response cut off by stop_reason "max_tokens" can end mid-block — either
// way there is nothing to parse.
func responseText(resp *sdk.Message) (string, error) {
	for _, block := range resp.Content {
		if block.Type == "text" && block.Text != "" {
			return block.Text, nil
		}
	}
	return "", fmt.Errorf("response has no text content (stop_reason=%s)", resp.StopReason)
}

// parseWireProposal parses raw as a wireProposal, with one repair attempt:
// if raw does not parse as-is (the model wrapped the JSON object in prose
// or a markdown fence despite the response contract saying not to), it
// retries once against the substring from raw's first '{' to its last '}'.
// A second failure is returned verbatim — this package makes exactly one
// repair attempt, never more, and never fabricates a Proposal in place of a
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
// last '}', inclusive — a deliberately simple repair heuristic for a model
// that wrapped its JSON object in prose or a markdown fence. It does not
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
// Kind requires are actually set. It does not check whether an "click"
// ActionID is still valid — that stale-action check is the loop's job
// (observe.Engine.Validate against the engine's CURRENT state), never a
// Provider's; see actor's package doc. ObservationSequence is never taken
// from the model's reply at all: for a "click" proposal it is always
// prompt.Observation.Sequence, the only observation the model could have
// seen this turn, matching actor.Proposal.ObservationSequence's own doc
// ("the Observation.Sequence the proposal was chosen from").
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

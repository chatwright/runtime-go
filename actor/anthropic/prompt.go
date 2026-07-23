package anthropic

import (
	"fmt"
	"strings"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/observe"
)

// promptContractVersion tags the shape renderPrompt produces and the
// response contract it asks for. Bump it whenever either changes materially
// (new sections, a changed response schema, reworded instructions that
// could shift model behaviour) — see README.md's cassette-workflow section
// for why that matters even though it does not affect a cassette entry's
// lookup key (actor.CassetteProvider keys on the canonical actor.Prompt
// JSON, not on this package's rendering of it).
const promptContractVersion = "chatwright-anthropic-prompt/v1"

// systemPrompt is renderPrompt's system-message half: fixed instructions
// and the response contract. It does not depend on the actor.Prompt being
// rendered, so it is a plain constant.
const systemPrompt = `You are Chatwright's autonomous conversational test actor (contract ` + promptContractVersion + `).

You test a real chat bot end to end by acting as its user: you choose exactly one next action toward the active task's success criteria, based only on what is currently visible. You never see platform-internal data (callback payloads, native message IDs) — only user-visible messages and the labelled actions attached to them.

Respond with EXACTLY one JSON object matching the supplied schema. No prose, no markdown code fence, nothing before or after the object.

Choose exactly one "kind":
  - "send-text": send free text as the user. Set "text" to that text; leave "action_id" empty.
  - "click": activate one of the actions listed under "Available actions" below, by its exact "id". Set "action_id" to that id; leave "text" empty. Never invent an id that is not listed there.
  - "task-done": the active task's success criteria are visibly met. Leave "text" and "action_id" empty.
  - "give-up": the active task cannot be completed by further action (a dead end, a bug, an unrecoverable error). Leave "text" and "action_id" empty.

Always set "rationale" to one short, honest sentence explaining the choice. It is recorded for human review — never private chain-of-thought, just enough for a developer to understand why you did this.

If "Recent history" below shows a proposal that was already marked invalid or produced no effect, do not repeat it verbatim — it did not work.`

// renderPrompt deterministically renders prompt into the system and user
// text sent to the Anthropic Messages API. It is a pure function of prompt
// — no clock, no randomness, no map iteration over prompt's own data — so
// the same actor.Prompt always renders identical text.
func renderPrompt(prompt actor.Prompt) (system, user string) {
	return systemPrompt, renderUserPrompt(prompt)
}

func renderUserPrompt(prompt actor.Prompt) string {
	var b strings.Builder

	b.WriteString("## Goal\n")
	fmt.Fprintf(&b, "ID: %s\n", prompt.GoalID)
	fmt.Fprintf(&b, "Title: %s\n", prompt.GoalTitle)
	if prompt.GoalDescription != "" {
		fmt.Fprintf(&b, "Description: %s\n", prompt.GoalDescription)
	}
	if len(prompt.Constraints) > 0 {
		b.WriteString("Constraints:\n")
		for _, c := range prompt.Constraints {
			fmt.Fprintf(&b, "- %s\n", c)
		}
	}

	b.WriteString("\n## Active task\n")
	fmt.Fprintf(&b, "ID: %s\n", prompt.TaskID)
	fmt.Fprintf(&b, "Title: %s\n", prompt.TaskTitle)
	fmt.Fprintf(&b, "Success criteria: %s\n", prompt.TaskSuccessCriteria)

	b.WriteString("\n")
	renderObservation(&b, prompt.Observation)

	b.WriteString("\n")
	renderHistory(&b, prompt.History)

	b.WriteString("\n## Response contract\n")
	b.WriteString("Reply with exactly one JSON object matching the schema: choose one \"kind\" of " +
		"send-text | click | task-done | give-up, fill only the fields that kind needs (leave the rest " +
		"as empty strings), and always set \"rationale\".\n")

	return b.String()
}

func renderObservation(b *strings.Builder, obs observe.Observation) {
	fmt.Fprintf(b, "## Current observation (sequence %d)\n", obs.Sequence)

	if len(obs.Messages) == 0 {
		b.WriteString("No visible messages yet.\n")
	} else {
		b.WriteString("Visible messages, oldest to newest:\n")
		for _, m := range obs.Messages {
			edited := ""
			if m.Edited {
				edited = " (edited)"
			}
			fmt.Fprintf(b, "- [%s] %s v%d%s: %q\n", m.Actor, m.ID, m.Version, edited, m.Text)
			for _, a := range m.Actions {
				fmt.Fprintf(b, "    available action: id=%q label=%q\n", a.ID, a.Label)
			}
		}
	}

	if len(obs.Changes) == 0 {
		b.WriteString("Changes since the previous observation: none (first observation).\n")
	} else {
		b.WriteString("Changes since the previous observation:\n")
		for _, c := range obs.Changes {
			fmt.Fprintf(b, "- %s: message %s (%s)\n", c.Kind, c.MessageID, c.Actor)
		}
	}
}

func renderHistory(b *strings.Builder, history []actor.LoopEvent) {
	if len(history) == 0 {
		b.WriteString("## Recent history\nNone yet — this is the first attempt at this task.\n")
		return
	}

	fmt.Fprintf(b, "## Recent history (last %d attempts, oldest first)\n", len(history))
	for i, ev := range history {
		fmt.Fprintf(b, "%d. proposed %s", i+1, describeProposal(ev.Proposal))
		if ev.Validation.Checked {
			fmt.Fprintf(b, "; validation=%s (%s)", ev.Validation.Verdict, ev.Validation.Reason)
		}
		fmt.Fprintf(b, "; outcome=%s", ev.Action.Kind)
		if ev.Action.Detail != "" {
			fmt.Fprintf(b, " (%s)", ev.Action.Detail)
		}
		b.WriteString("\n")
	}
}

func describeProposal(p actor.Proposal) string {
	switch p.Kind {
	case actor.ProposeSendText:
		return fmt.Sprintf("send-text %q", p.Text)
	case actor.ProposeClick:
		return fmt.Sprintf("click action_id=%q", p.ActionID)
	default:
		return p.Kind.String()
	}
}

// responseJSONSchema is the JSON schema the Anthropic Messages API enforces
// server-side via output_config.format (structured outputs — see
// provider.go's Propose): the model's reply is guaranteed to be exactly one
// JSON object matching this shape, which is what lets response.go parse it
// with a single json.Unmarshal in the common case. Every field is required
// — Anthropic's structured-outputs schema support has no notion of
// "required only when kind is X" (no if/then), so the model always emits
// "text" and "action_id", using an empty string for whichever one the
// chosen "kind" does not need.
var responseJSONSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"kind": map[string]any{
			"type":        "string",
			"enum":        []string{"send-text", "click", "task-done", "give-up"},
			"description": "The chosen action kind.",
		},
		"text": map[string]any{
			"type":        "string",
			"description": `The text to send as the user. Non-empty when kind is "send-text"; empty otherwise.`,
		},
		"action_id": map[string]any{
			"type":        "string",
			"description": `The exact id of an action listed under "Available actions" in the prompt. Non-empty when kind is "click"; empty otherwise.`,
		},
		"rationale": map[string]any{
			"type":        "string",
			"description": "One short, honest sentence explaining the choice, for human review.",
		},
	},
	"required":             []string{"kind", "text", "action_id", "rationale"},
	"additionalProperties": false,
}

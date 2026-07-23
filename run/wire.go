package run

// This file is the runtime→wire conversion seam: every converter below is a
// mechanical, field-by-field mapping from one of this module's internal
// types (platform.JournalEntry, goal.Goal, actor.LoopEvent,
// observe.Observation, campaign.Report, datastate.Evidence) to its
// chatwright.dev/sdk wire equivalent. The sdk owns every wire shape — no
// field is renamed, reordered, invented or dropped here, and every
// string-enum conversion is a plain cast over an identical wire value — so
// the JSON a converted value marshals to is byte-identical to what the
// pre-split bundle package produced by carrying the runtime values verbatim.

import (
	"sort"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/campaign"
	"chatwright.dev/runtime/datastate"
	"chatwright.dev/runtime/goal"
	"chatwright.dev/runtime/observe"
	"chatwright.dev/runtime/platform"
	"chatwright.dev/sdk"
)

// WireJournal packages one chat's journal — the entries returned by
// platform.Emulator.Journal — as the sdk.ChatJournal a run-bundle Run.Chats
// roster carries, converting each platform.JournalEntry to its sdk wire
// equivalent. It is the helper callers assembling a Run's Chats (for
// SingleAIGoalRun or AssembleBundleRun) build each per-chat entry with.
func WireJournal(chatID int64, entries []platform.JournalEntry) sdk.ChatJournal {
	return sdk.ChatJournal{ChatID: chatID, Entries: wireEntries(entries)}
}

// wireSlice maps a runtime slice to its wire counterpart element by element,
// preserving nilness: a nil input stays nil (so encoding/json renders
// exactly what the runtime produced — null vs [] — as the pre-split bundle
// package did by carrying runtime slices verbatim).
func wireSlice[In, Out any](in []In, conv func(In) Out) []Out {
	if in == nil {
		return nil
	}
	out := make([]Out, len(in))
	for i, v := range in {
		out[i] = conv(v)
	}
	return out
}

// wireEntries is a mechanical wire mapping ([]platform.JournalEntry →
// []sdk.JournalEntry); the sdk owns the shape.
func wireEntries(entries []platform.JournalEntry) []sdk.JournalEntry {
	return wireSlice(entries, wireJournalEntry)
}

// wireJournalEntry is a mechanical wire mapping (platform.JournalEntry →
// sdk.JournalEntry); the sdk owns the shape.
func wireJournalEntry(e platform.JournalEntry) sdk.JournalEntry {
	return sdk.JournalEntry{
		Direction:    sdk.Direction(e.Direction),
		Kind:         sdk.JournalEntryKind(e.Kind),
		MessageID:    e.MessageID,
		RefMessageID: e.RefMessageID,
		Version:      e.Version,
		Text:         e.Text,
		Actions:      wireSlice(e.Actions, wireActionRow),
		Method:       e.Method,
		At:           e.At,
		FromID:       e.FromID,
	}
}

// wireActionRow is a mechanical wire mapping (one row of platform.Action →
// []sdk.Action); the sdk owns the shape.
func wireActionRow(row []platform.Action) []sdk.Action {
	return wireSlice(row, wireAction)
}

// wireAction is a mechanical wire mapping (platform.Action → sdk.Action);
// the sdk owns the shape.
func wireAction(a platform.Action) sdk.Action {
	return sdk.Action{Label: a.Label, ID: a.ID, URL: a.URL}
}

// wireGoal is a mechanical wire mapping (goal.Goal → sdk.Goal); the sdk owns
// the shape.
func wireGoal(g goal.Goal) sdk.Goal {
	return sdk.Goal{
		ID:          g.ID,
		Title:       g.Title,
		Description: g.Description,
		Tasks:       wireSlice(g.Tasks, wireTask),
		Constraints: g.Constraints,
		Budgets:     wireBudgets(g.Budgets),
	}
}

// wireTask is a mechanical wire mapping (goal.Task → sdk.Task); the sdk owns
// the shape.
func wireTask(t goal.Task) sdk.Task {
	return sdk.Task{
		ID:              t.ID,
		Title:           t.Title,
		DependsOn:       t.DependsOn,
		SuccessCriteria: t.SuccessCriteria,
		Milestones:      t.Milestones,
	}
}

// wireBudgets is a mechanical wire mapping (goal.Budgets → sdk.Budgets); the
// sdk owns the shape.
func wireBudgets(b goal.Budgets) sdk.Budgets {
	return sdk.Budgets{
		MaxSteps:            b.MaxSteps,
		MaxDuration:         b.MaxDuration,
		MaxRepeatedFailures: b.MaxRepeatedFailures,
		MaxCost:             b.MaxCost,
	}
}

// wireLoopEvents is a mechanical wire mapping ([]actor.LoopEvent →
// []sdk.LoopEvent); the sdk owns the shape.
func wireLoopEvents(events []actor.LoopEvent) []sdk.LoopEvent {
	return wireSlice(events, wireLoopEvent)
}

// wireLoopEvent is a mechanical wire mapping (actor.LoopEvent →
// sdk.LoopEvent); the sdk owns the shape.
func wireLoopEvent(e actor.LoopEvent) sdk.LoopEvent {
	return sdk.LoopEvent{
		Index:               e.Index,
		At:                  e.At,
		TaskID:              e.TaskID,
		ObservationSequence: e.ObservationSequence,
		Proposal:            wireProposal(e.Proposal),
		Usage:               wireUsage(e.Usage),
		Validation:          wireValidationOutcome(e.Validation),
		Action:              wireActionOutcome(e.Action),
		ProposeError:        e.ProposeError,
	}
}

// wireProposal is a mechanical wire mapping (actor.Proposal → sdk.Proposal);
// the sdk owns the shape.
func wireProposal(p actor.Proposal) sdk.Proposal {
	return sdk.Proposal{
		Kind:                sdk.ProposalKind(p.Kind),
		Text:                p.Text,
		ActionID:            p.ActionID,
		ObservationSequence: p.ObservationSequence,
		Rationale:           p.Rationale,
	}
}

// wireUsage is a mechanical wire mapping (actor.Usage → sdk.Usage); the sdk
// owns the shape.
func wireUsage(u actor.Usage) sdk.Usage {
	return sdk.Usage{
		Model:        u.Model,
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		Latency:      u.Latency,
		Cost:         u.Cost,
	}
}

// wireValidationOutcome is a mechanical wire mapping (actor.ValidationOutcome
// → sdk.ValidationOutcome); the sdk owns the shape.
func wireValidationOutcome(v actor.ValidationOutcome) sdk.ValidationOutcome {
	return sdk.ValidationOutcome{
		Checked: v.Checked,
		Verdict: sdk.Verdict(v.Verdict),
		Reason:  v.Reason,
	}
}

// wireActionOutcome is a mechanical wire mapping (actor.ActionOutcome →
// sdk.ActionOutcome); the sdk owns the shape.
func wireActionOutcome(o actor.ActionOutcome) sdk.ActionOutcome {
	return sdk.ActionOutcome{Kind: sdk.ActionOutcomeKind(o.Kind), Detail: o.Detail}
}

// wireRetainedObservations converts observations — as returned by
// actor.Loop.Observations — into an sdk.AIGoalSection.Observations-ready
// slice, ordered ascending by Sequence. It absorbs the pre-split bundle
// package's SortObservations: the sort lives here (inside SingleAIGoalRun
// and the ai-goal part assembly) rather than as an exported helper, so the
// section's JSON stays chronologically readable regardless of
// encoding/json's own map-key ordering. Like SortObservations, it always
// returns a non-nil slice.
func wireRetainedObservations(observations map[int64]observe.Observation) []sdk.RetainedObservation {
	out := make([]sdk.RetainedObservation, 0, len(observations))
	for seq, obs := range observations {
		out = append(out, sdk.RetainedObservation{Sequence: seq, Observation: wireObservation(obs)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return out
}

// wireObservation is a mechanical wire mapping (observe.Observation →
// sdk.Observation); the sdk owns the shape.
func wireObservation(o observe.Observation) sdk.Observation {
	return sdk.Observation{
		Sequence:         o.Sequence,
		PreviousSequence: o.PreviousSequence,
		Chat:             wireChatRef(o.Chat),
		Messages:         wireSlice(o.Messages, wireVisibleMessage),
		Changes:          wireSlice(o.Changes, wireChange),
	}
}

// wireChatRef is a mechanical wire mapping (observe.ChatRef → sdk.ChatRef);
// the sdk owns the shape.
func wireChatRef(c observe.ChatRef) sdk.ChatRef {
	return sdk.ChatRef{ChatID: c.ChatID}
}

// wireVisibleMessage is a mechanical wire mapping (observe.VisibleMessage →
// sdk.VisibleMessage); the sdk owns the shape. observe.Actor is
// sdk.MessageActor on the wire — renamed there, Go name only.
func wireVisibleMessage(m observe.VisibleMessage) sdk.VisibleMessage {
	return sdk.VisibleMessage{
		ID:      m.ID,
		Version: m.Version,
		Edited:  m.Edited,
		Actor:   sdk.MessageActor(m.Actor),
		Text:    m.Text,
		Actions: wireSlice(m.Actions, wireAvailableAction),
	}
}

// wireAvailableAction is a mechanical wire mapping (observe.AvailableAction
// → sdk.AvailableAction); the sdk owns the shape.
func wireAvailableAction(a observe.AvailableAction) sdk.AvailableAction {
	return sdk.AvailableAction{ID: a.ID, Label: a.Label, SeenAt: a.SeenAt}
}

// wireChange is a mechanical wire mapping (observe.Change → sdk.Change); the
// sdk owns the shape.
func wireChange(c observe.Change) sdk.Change {
	return sdk.Change{
		Kind:            sdk.ChangeKind(c.Kind),
		MessageID:       c.MessageID,
		Actor:           sdk.MessageActor(c.Actor),
		PreviousVersion: c.PreviousVersion,
		Version:         c.Version,
	}
}

// wireReport is a mechanical wire mapping (campaign.Report → sdk.Report);
// the sdk owns the shape.
func wireReport(r campaign.Report) sdk.Report {
	return sdk.Report{
		SchemaVersion: r.SchemaVersion,
		GoalID:        r.GoalID,
		GoalTitle:     r.GoalTitle,
		StopReason:    r.StopReason,
		Steps:         r.Steps,
		Cost:          r.Cost,
		Elapsed:       r.Elapsed,
		Tasks:         wireSlice(r.Tasks, wireTaskOutcome),
		Findings:      wireSlice(r.Findings, wireFinding),
		Usage:         wireAggregateUsage(r.Usage),
	}
}

// wireTaskOutcome is a mechanical wire mapping (campaign.TaskOutcome →
// sdk.TaskOutcome); the sdk owns the shape.
func wireTaskOutcome(t campaign.TaskOutcome) sdk.TaskOutcome {
	return sdk.TaskOutcome{
		TaskID:          t.TaskID,
		Title:           t.Title,
		SuccessCriteria: t.SuccessCriteria,
		Status:          t.Status,
		Attempted:       t.Attempted,
		FailureCount:    t.FailureCount,
	}
}

// wireFinding is a mechanical wire mapping (campaign.Finding → sdk.Finding);
// the sdk owns the shape.
func wireFinding(f campaign.Finding) sdk.Finding {
	return sdk.Finding{
		Kind:       sdk.FindingKind(f.Kind),
		TaskID:     f.TaskID,
		Summary:    f.Summary,
		Evidence:   wireFindingEvidence(f.Evidence),
		Confidence: f.Confidence,
	}
}

// wireFindingEvidence is a mechanical wire mapping (campaign.Evidence →
// sdk.FindingEvidence); the sdk owns the shape. campaign.Evidence is
// sdk.FindingEvidence on the wire — renamed there, Go name only.
func wireFindingEvidence(e campaign.Evidence) sdk.FindingEvidence {
	return sdk.FindingEvidence{
		ObservationSequences: e.ObservationSequences,
		LoopEventIndexes:     e.LoopEventIndexes,
	}
}

// wireAggregateUsage is a mechanical wire mapping (campaign.AggregateUsage →
// sdk.AggregateUsage); the sdk owns the shape.
func wireAggregateUsage(u campaign.AggregateUsage) sdk.AggregateUsage {
	return sdk.AggregateUsage{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		Cost:         u.Cost,
		CallCount:    u.CallCount,
	}
}

// wireDataStateEvidences is a mechanical wire mapping ([]datastate.Evidence
// → []sdk.DataStateEvidence); the sdk owns the shape.
func wireDataStateEvidences(evidence []datastate.Evidence) []sdk.DataStateEvidence {
	return wireSlice(evidence, wireDataStateEvidence)
}

// wireDataStateEvidence is a mechanical wire mapping (datastate.Evidence →
// sdk.DataStateEvidence); the sdk owns the shape. datastate.Evidence is
// sdk.DataStateEvidence on the wire — renamed there, Go name only.
func wireDataStateEvidence(e datastate.Evidence) sdk.DataStateEvidence {
	return sdk.DataStateEvidence{
		Name:            e.Name,
		AttachmentPoint: sdk.AttachmentPoint(e.AttachmentPoint),
		Holder:          e.Holder,
		Query:           e.Query,
		Params:          e.Params,
		Outcome:         sdk.Outcome(e.Outcome),
		FailureMessage:  e.FailureMessage,
		TotalRows:       e.TotalRows,
		ReturnedRows:    e.ReturnedRows,
		Truncated:       e.Truncated,
		Preview:         wireSlice(e.Preview, wireRow),
		RedactedFields:  e.RedactedFields,
		ExcludedFields:  e.ExcludedFields,
	}
}

// wireRow is a mechanical wire mapping (datastate.Row → sdk.Row); the sdk
// owns the shape. Both are map[string]any — the cast changes the Go type
// only, never the data.
func wireRow(r datastate.Row) sdk.Row {
	return sdk.Row(r)
}

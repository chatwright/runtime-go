package campaign

import (
	"fmt"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/goal"
	"chatwright.dev/runtime/observe"
)

// AssembleInput is everything Assemble needs to build a Report: the Goal a
// campaign ran, a goal.CampaignSnapshot of the state it reached, and every
// actor.LoopEvent the loop recorded while running it.
type AssembleInput struct {
	Goal     goal.Goal
	Campaign goal.CampaignSnapshot
	Events   []actor.LoopEvent

	// CallerFindings are findings this slice's mechanics cannot derive on
	// their own — chiefly FindingVerifiedDefect: "the actor acted and the
	// observed outcome was wrong" requires deterministic or DTQL evidence
	// (a later slice) or some other judgement outside this package's
	// mechanical rules. Assemble includes them verbatim, appended after its
	// own derived findings — this is the caller's seam to plug verification
	// in; Assemble does not validate or reclassify them.
	CallerFindings []Finding
}

// Assemble builds a Report from in. Task outcomes are read straight off
// in.Campaign/in.Goal. Findings are derived mechanically per task:
//
//   - a task that reached TaskFailed or TaskBlocked, whose recorded history
//     for that task shows a stale/invalid proposal or an unresolvable
//     action, becomes FindingAINavigationFailure — the campaign never
//     showed the bot itself to be at fault, only that the actor could not
//     complete the task;
//   - a task never attempted (still TaskPending when the campaign stopped),
//     or attempted but never concluded (non-terminal despite at least one
//     recorded event — e.g. a budget interrupted it mid-task), becomes
//     FindingCoverageGap;
//   - a cleanly failed/blocked task (no stale/invalid history) produces no
//     mechanical finding: these mechanics cannot tell a genuine product
//     defect from a persona/constraint dead end, so a verified-defect claim
//     for it must come through AssembleInput.CallerFindings instead.
//
// A TaskCompleted task never produces a finding by itself; its success
// already lives in its TaskOutcome.
func Assemble(in AssembleInput) Report {
	eventsByTask := groupEventsByTask(in.Events)

	tasks := make([]TaskOutcome, 0, len(in.Goal.Tasks))
	findings := make([]Finding, 0, len(in.Goal.Tasks)+len(in.CallerFindings))
	for _, task := range in.Goal.Tasks {
		events := eventsByTask[task.ID]
		attempted := len(events) > 0
		status := in.Campaign.Statuses[task.ID]

		tasks = append(tasks, TaskOutcome{
			TaskID:          task.ID,
			Title:           task.Title,
			SuccessCriteria: task.SuccessCriteria,
			Status:          string(status),
			Attempted:       attempted,
			FailureCount:    in.Campaign.Failures[task.ID],
		})

		if finding, ok := deriveFinding(task.ID, status, attempted, events); ok {
			findings = append(findings, finding)
		}
	}
	findings = append(findings, in.CallerFindings...)

	return Report{
		SchemaVersion: ReportSchemaVersion,
		GoalID:        in.Goal.ID,
		GoalTitle:     in.Goal.Title,
		StopReason:    string(in.Campaign.StopReason),
		Steps:         in.Campaign.Steps,
		Cost:          in.Campaign.Cost,
		Elapsed:       in.Campaign.Elapsed,
		Tasks:         tasks,
		Findings:      findings,
		Usage:         aggregateUsage(in.Events),
	}
}

// groupEventsByTask partitions events by TaskID, preserving each task's
// original relative order.
func groupEventsByTask(events []actor.LoopEvent) map[string][]actor.LoopEvent {
	byTask := make(map[string][]actor.LoopEvent)
	for _, e := range events {
		byTask[e.TaskID] = append(byTask[e.TaskID], e)
	}
	return byTask
}

// deriveFinding applies Assemble's mechanical classification rules to one
// task.
func deriveFinding(taskID string, status goal.TaskStatus, attempted bool, events []actor.LoopEvent) (Finding, bool) {
	switch {
	case !attempted:
		if status.Terminal() {
			// A terminal status with zero recorded events is not something
			// this slice's loop produces (Complete/Fail always follow at
			// least one LoopEvent) — but a caller building Campaign/Events
			// by hand could construct it; treat it the same as a coverage
			// gap rather than panic or silently drop it.
			return coverageGapFinding(taskID, "task has a terminal status but no recorded loop events to evidence it"), true
		}
		return coverageGapFinding(taskID, "task was never attempted before the campaign stopped"), true

	case status == goal.TaskFailed || status == goal.TaskBlocked:
		if seqs, idxs, ok := navigationFailureEvidence(events); ok {
			return Finding{
				Kind:       FindingAINavigationFailure,
				TaskID:     taskID,
				Summary:    fmt.Sprintf("task %q did not complete; its history shows the actor's own proposals going stale or unresolvable, not a confirmed bot defect", taskID),
				Evidence:   Evidence{ObservationSequences: seqs, LoopEventIndexes: idxs},
				Confidence: "mechanical",
			}, true
		}
		return Finding{}, false // a clean failure: mechanics can't classify it further; see AssembleInput.CallerFindings.

	case !status.Terminal():
		// Attempted but interrupted mid-task (e.g. a budget stopped the
		// campaign while this task was still Active).
		last := events[len(events)-1]
		return Finding{
			Kind:     FindingCoverageGap,
			TaskID:   taskID,
			Summary:  fmt.Sprintf("task %q was still in progress when the campaign stopped; its outcome is unverified", taskID),
			Evidence: Evidence{ObservationSequences: []int64{last.ObservationSequence}, LoopEventIndexes: []int{last.Index}},
		}, true

	default:
		return Finding{}, false // TaskCompleted: no finding, see TaskOutcome.
	}
}

// coverageGapFinding builds an evidence-free FindingCoverageGap for taskID —
// used when a task was never attempted at all, so there is nothing to link.
func coverageGapFinding(taskID, summary string) Finding {
	return Finding{Kind: FindingCoverageGap, TaskID: taskID, Summary: summary}
}

// navigationFailureEvidence scans a task's events for a stale/invalid
// validation or an unresolvable action, returning the observation sequences
// and loop-event indexes of every such event found. ok is false if none was
// found (a "clean" failure this package's mechanics cannot further
// classify).
func navigationFailureEvidence(events []actor.LoopEvent) (sequences []int64, indexes []int, ok bool) {
	for _, e := range events {
		stale := e.Validation.Checked && e.Validation.Verdict == observe.VerdictStale
		invalid := e.Action.Kind == actor.ActionSkippedInvalid || e.Action.Kind == actor.ActionResolutionFailed
		if stale || invalid {
			sequences = append(sequences, e.ObservationSequence)
			indexes = append(indexes, e.Index)
		}
	}
	return sequences, indexes, len(indexes) > 0
}

// aggregateUsage sums the actor.Usage of every event.
func aggregateUsage(events []actor.LoopEvent) AggregateUsage {
	var agg AggregateUsage
	for _, e := range events {
		agg.InputTokens += e.Usage.InputTokens
		agg.OutputTokens += e.Usage.OutputTokens
		if e.Usage.Cost != nil {
			agg.Cost += *e.Usage.Cost
		}
		agg.CallCount++
	}
	return agg
}

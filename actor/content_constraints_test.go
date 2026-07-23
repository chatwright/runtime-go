package actor_test

import (
	"context"
	"testing"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/campaign"
	"chatwright.dev/runtime/goal"
	"chatwright.dev/runtime/observe"
	"chatwright.dev/runtime/platform"
)

// TestContentConstraintViolationBlockedRecordedAndReprompted is
// proposal-content-constraints' MVP proof: a scripted provider proposing
// "add a plasma TV" against a groceries-vocabulary task is blocked BEFORE
// it ever reaches the bot (never a SubmitText call), recorded as
// actor.ActionBlockedConstraintViolation, counted toward NonProgressLimit
// like any other invalid proposal, and re-prompted — the next (valid)
// proposal succeeds. campaign.Assemble then classifies the violation as
// campaign.FindingConstraintViolation, and the final journal (via fakeChat,
// standing in for the platform) contains only the allowed send.
func TestContentConstraintViolationBlockedRecordedAndReprompted(t *testing.T) {
	fc := newFakeChat()
	fc.seedBotMessage("What would you like to add?", nil)
	fc.queueBotReply("Added milk.", nil) // consumed by the one successful send only

	engine := observe.NewEngine(fc, observe.ChatRef{ChatID: testChatID})
	clock := newFakeClock()
	groceries := goal.ContentRules{Vocabulary: []string{"milk", "eggs", "bread", "cheese"}}
	g := goal.Goal{ID: "g", Tasks: []goal.Task{{
		ID: "add-items", SuccessCriteria: "an allowed grocery item is added",
		ContentRules: groceries,
	}}, Budgets: goal.Budgets{MaxRepeatedFailures: 5}}
	campaignState := mustCampaign(t, g, clock.now)

	provider := actor.NewScriptedProvider(actor.Usage{Model: "scripted"},
		actor.Proposal{Kind: actor.ProposeSendText, Text: "add a plasma TV", Rationale: "off-domain — should be blocked"},
		actor.Proposal{Kind: actor.ProposeSendText, Text: "add milk", Rationale: "on-domain — should succeed"},
		actor.Proposal{Kind: actor.ProposeTaskDone, Rationale: "milk was added"},
	)
	lp := mustLoop(t, provider, engine, fc, campaignState, g, actor.Config{ChatID: testChatID, User: testUser, Now: clock.now})

	result, err := lp.RunTask(context.Background(), "add-items")
	if err != nil {
		t.Fatalf("RunTask() error = %v", err)
	}
	if result.Status != goal.TaskCompleted {
		t.Fatalf("result.Status = %v, want TaskCompleted", result.Status)
	}

	events := lp.Events()
	if len(events) != 3 {
		t.Fatalf("recorded %d events, want 3 (blocked, valid, task-done): %+v", len(events), events)
	}
	if events[0].Action.Kind != actor.ActionBlockedConstraintViolation {
		t.Fatalf("event 0 action = %v, want ActionBlockedConstraintViolation", events[0].Action.Kind)
	}
	if events[0].Action.Detail == "" {
		t.Fatal("event 0 action detail is empty, want a reason naming the violated rule")
	}
	if events[1].Action.Kind != actor.ActionExecuted {
		t.Fatalf("event 1 action = %v, want ActionExecuted", events[1].Action.Kind)
	}
	if events[2].Action.Kind != actor.ActionTaskCompleted {
		t.Fatalf("event 2 action = %v, want ActionTaskCompleted", events[2].Action.Kind)
	}

	// The blocked proposal must never have reached the platform: exactly
	// one SubmitText call (the allowed one).
	if fc.submitTextCalls != 1 {
		t.Fatalf("SubmitText called %d times, want exactly 1 — the blocked proposal must not act", fc.submitTextCalls)
	}

	// The final journal contains only the allowed send — never the
	// off-domain proposal's text.
	entries, err := fc.Journal(testChatID)
	if err != nil {
		t.Fatalf("Journal() error = %v", err)
	}
	for _, e := range entries {
		if e.Direction == platform.DirectionUser && e.Kind == platform.JournalEntryMessage {
			if e.Text != "add milk" {
				t.Fatalf("journal contains a user send %q, want only the allowed \"add milk\"", e.Text)
			}
		}
	}

	report := campaign.Assemble(campaign.AssembleInput{Goal: g, Campaign: campaignState.Snapshot(), Events: events})
	var violations []campaign.Finding
	for _, f := range report.Findings {
		if f.Kind == campaign.FindingConstraintViolation {
			violations = append(violations, f)
		}
	}
	if len(violations) != 1 {
		t.Fatalf("report.Findings constraint-violation count = %d, want exactly 1: %+v", len(violations), report.Findings)
	}
	if violations[0].TaskID != "add-items" || len(violations[0].Evidence.LoopEventIndexes) != 1 || violations[0].Evidence.LoopEventIndexes[0] != 0 {
		t.Fatalf("violation finding = %+v, want it scoped to add-items and linked to event index 0", violations[0])
	}
}

// TestContentConstraintViolationCountsTowardNonProgress proves a blocked
// proposal counts toward Config.NonProgressLimit exactly like any other
// invalid proposal — repeated violations stop the campaign via the loop's
// own non-progress detection, never silently tolerated.
func TestContentConstraintViolationCountsTowardNonProgress(t *testing.T) {
	fc := newFakeChat()
	fc.seedBotMessage("What would you like to add?", nil)

	engine := observe.NewEngine(fc, observe.ChatRef{ChatID: testChatID})
	clock := newFakeClock()
	g := goal.Goal{ID: "g", Tasks: []goal.Task{{
		ID: "add-items", ContentRules: goal.ContentRules{Vocabulary: []string{"milk"}},
	}}}
	campaignState := mustCampaign(t, g, clock.now)

	provider := actor.NewScriptedProvider(actor.Usage{Model: "scripted"},
		actor.Proposal{Kind: actor.ProposeSendText, Text: "add a TV"},
		actor.Proposal{Kind: actor.ProposeSendText, Text: "add a DVD"},
	)
	lp := mustLoop(t, provider, engine, fc, campaignState, g, actor.Config{ChatID: testChatID, User: testUser, Now: clock.now, NonProgressLimit: 2})

	result, err := lp.RunTask(context.Background(), "add-items")
	if err != nil {
		t.Fatalf("RunTask() error = %v", err)
	}
	if !result.NonProgress {
		t.Fatal("result.NonProgress = false, want true — two blocked proposals in a row must trip NonProgressLimit")
	}
	if fc.submitTextCalls != 0 {
		t.Fatalf("SubmitText called %d times, want 0 — neither blocked proposal may act", fc.submitTextCalls)
	}
}

// TestGoalLevelContentRulesApplyWhenTaskDeclaresNone proves
// goal.EffectiveContentRules' "task overriding goal" resolution end to
// end: a task with no ContentRules of its own inherits its Goal's.
func TestGoalLevelContentRulesApplyWhenTaskDeclaresNone(t *testing.T) {
	fc := newFakeChat()
	fc.seedBotMessage("What would you like to add?", nil)

	engine := observe.NewEngine(fc, observe.ChatRef{ChatID: testChatID})
	clock := newFakeClock()
	g := goal.Goal{
		ID:           "g",
		ContentRules: goal.ContentRules{Vocabulary: []string{"milk"}},
		Tasks:        []goal.Task{{ID: "add-items"}}, // no task-level rules
	}
	campaignState := mustCampaign(t, g, clock.now)

	provider := actor.NewScriptedProvider(actor.Usage{Model: "scripted"},
		actor.Proposal{Kind: actor.ProposeSendText, Text: "add a TV"},
	)
	lp := mustLoop(t, provider, engine, fc, campaignState, g, actor.Config{ChatID: testChatID, User: testUser, Now: clock.now, NonProgressLimit: 1})

	if _, err := lp.RunTask(context.Background(), "add-items"); err != nil {
		t.Fatalf("RunTask() error = %v", err)
	}
	events := lp.Events()
	if len(events) != 1 || events[0].Action.Kind != actor.ActionBlockedConstraintViolation {
		t.Fatalf("events = %+v, want exactly one ActionBlockedConstraintViolation (goal-level rules applied)", events)
	}
}

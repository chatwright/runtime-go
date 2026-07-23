package actor_test

import (
	"context"
	"testing"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/campaign"
	"chatwright.dev/runtime/goal"
	"chatwright.dev/runtime/observe"
)

// ackCriteria is a deterministic goal.Criteria: it holds the moment a bot
// message with the given text is visible in the observation — the
// "deterministic conversation predicate" shape
// spec/ideas/evidence-defined-completion.md describes, no DTQL/executor
// needed for this proof.
func ackCriteria(wantText string) goal.Criteria {
	return func(_ context.Context, obs observe.Observation) (bool, error) {
		for _, m := range obs.Messages {
			if m.Actor == observe.ActorBot && m.Text == wantText {
				return true, nil
			}
		}
		return false, nil
	}
}

// TestEvidenceDefinedCompletionStopsAtMetMomentAndRecordsOvershoot is
// evidence-defined-completion's MVP proof (a): a scripted provider that has
// ANOTHER proposal ready to go after the criteria-meeting action executes —
// modelling a model that "keeps proposing after criteria hold" — stops the
// task at the exact moment its Criteria are met, with the campaign's
// StopReason naming evidence (goal.StopGoalMetByEvidence) rather than the
// actor's own task-done claim, and the excess proposal is requested once
// (the overshoot probe), recorded, and NEVER submitted to the platform —
// then classified as campaign.FindingActorOvershoot at report assembly.
func TestEvidenceDefinedCompletionStopsAtMetMomentAndRecordsOvershoot(t *testing.T) {
	fc := newFakeChat()
	fc.seedBotMessage("Hello", nil)
	fc.queueBotReply("ack", nil) // the ONE reply that satisfies criteria

	engine := observe.NewEngine(fc, observe.ChatRef{ChatID: testChatID})
	clock := newFakeClock()
	g := goal.Goal{ID: "g", Tasks: []goal.Task{{
		ID: "t1", SuccessCriteria: "the bot acknowledges",
		Criteria: ackCriteria("ack"),
	}}}
	campaignState := mustCampaign(t, g, clock.now)

	// The script has TWO entries: the action that satisfies criteria, and
	// one more the model was ready to send afterwards — the overshoot
	// probe consumes it, proving the "keeps proposing" model shape without
	// ever letting it reach the platform.
	provider := actor.NewScriptedProvider(actor.Usage{Model: "scripted"},
		actor.Proposal{Kind: actor.ProposeSendText, Text: "trigger", Rationale: "start the exchange"},
		actor.Proposal{Kind: actor.ProposeSendText, Text: "thanks!", Rationale: "the model wanted to keep going"},
	)
	lp := mustLoop(t, provider, engine, fc, campaignState, g, actor.Config{ChatID: testChatID, User: testUser, Now: clock.now})

	result, err := lp.RunTask(context.Background(), "t1")
	if err != nil {
		t.Fatalf("RunTask() error = %v", err)
	}
	if result.Status != goal.TaskCompleted {
		t.Fatalf("result.Status = %v, want TaskCompleted", result.Status)
	}
	if !result.Stopped {
		t.Fatal("result.Stopped = false, want true (the campaign's only task just completed)")
	}
	if reason, ok := campaignState.StopReason(); !ok || reason != goal.StopGoalMetByEvidence {
		t.Fatalf("StopReason() = %v, %v, want StopGoalMetByEvidence, true", reason, ok)
	}

	events := lp.Events()
	if len(events) != 2 {
		t.Fatalf("recorded %d events, want exactly 2 (the completing action + the overshoot probe): %+v", len(events), events)
	}
	if events[0].Action.Kind != actor.ActionExecuted {
		t.Fatalf("event 0 action = %v, want ActionExecuted (the criteria-meeting action)", events[0].Action.Kind)
	}
	if events[1].Action.Kind != actor.ActionOvershootProbe {
		t.Fatalf("event 1 action = %v, want ActionOvershootProbe", events[1].Action.Kind)
	}
	if events[1].Proposal.Text != "thanks!" {
		t.Fatalf("event 1 proposal text = %q, want %q (the excess proposal, recorded verbatim)", events[1].Proposal.Text, "thanks!")
	}

	// The excess proposal must never have reached the platform: exactly one
	// SubmitText call (the criteria-meeting one).
	if fc.submitTextCalls != 1 {
		t.Fatalf("SubmitText called %d times, want exactly 1 — the overshoot probe's proposal must never be submitted", fc.submitTextCalls)
	}

	report := campaign.Assemble(campaign.AssembleInput{Goal: g, Campaign: campaignState.Snapshot(), Events: events})
	if report.StopReason != string(goal.StopGoalMetByEvidence) {
		t.Fatalf("report.StopReason = %q, want %q", report.StopReason, goal.StopGoalMetByEvidence)
	}
	var overshoot []campaign.Finding
	for _, f := range report.Findings {
		if f.Kind == campaign.FindingActorOvershoot {
			overshoot = append(overshoot, f)
		}
	}
	if len(overshoot) != 1 {
		t.Fatalf("report.Findings actor-overshoot count = %d, want exactly 1: %+v", len(overshoot), report.Findings)
	}
	if overshoot[0].TaskID != "t1" || len(overshoot[0].Evidence.LoopEventIndexes) != 1 || overshoot[0].Evidence.LoopEventIndexes[0] != 1 {
		t.Fatalf("overshoot finding = %+v, want it scoped to t1 and linked to event index 1", overshoot[0])
	}
}

// TestPrematureTaskDoneWhileCriteriaFailStillYieldsNavigationFailure is
// evidence-defined-completion's MVP proof (b): when a task declares
// Criteria and the actor proposes task-done before they hold, the
// proposal is recorded (never accepted) and the task continues — the SAME
// "invalid proposal, recorded and re-prompted" mechanism
// TestInvalidProposalIsRecordedAndReprompted already proves for a stale
// click — so if the task later ends up unresolved (here: an explicit
// give-up), campaign.Assemble's PRE-EXISTING ai-navigation-failure
// classification applies completely unchanged.
func TestPrematureTaskDoneWhileCriteriaFailStillYieldsNavigationFailure(t *testing.T) {
	fc := newFakeChat()
	fc.seedBotMessage("Hello", nil)

	engine := observe.NewEngine(fc, observe.ChatRef{ChatID: testChatID})
	clock := newFakeClock()
	g := goal.Goal{ID: "g", Tasks: []goal.Task{{
		ID: "t1", SuccessCriteria: "the bot acknowledges",
		Criteria: ackCriteria("never appears"), // never holds in this script
	}}}
	campaignState := mustCampaign(t, g, clock.now)

	provider := actor.NewScriptedProvider(actor.Usage{Model: "scripted"},
		actor.Proposal{Kind: actor.ProposeTaskDone, Rationale: "premature: criteria were never actually checked by the model"},
		actor.Proposal{Kind: actor.ProposeGiveUp, Rationale: "gives up after being corrected"},
	)
	lp := mustLoop(t, provider, engine, fc, campaignState, g, actor.Config{ChatID: testChatID, User: testUser, Now: clock.now})

	result, err := lp.RunTask(context.Background(), "t1")
	if err != nil {
		t.Fatalf("RunTask() error = %v", err)
	}
	if result.Status != goal.TaskFailed {
		t.Fatalf("result.Status = %v, want TaskFailed", result.Status)
	}

	events := lp.Events()
	if len(events) != 2 {
		t.Fatalf("recorded %d events, want exactly 2 (rejected task-done + give-up): %+v", len(events), events)
	}
	if events[0].Action.Kind != actor.ActionSkippedInvalid {
		t.Fatalf("event 0 action = %v, want ActionSkippedInvalid (the premature task-done, rejected)", events[0].Action.Kind)
	}
	if events[1].Action.Kind != actor.ActionTaskGivenUp {
		t.Fatalf("event 1 action = %v, want ActionTaskGivenUp", events[1].Action.Kind)
	}

	// The rejected proposal must never have completed the task.
	if fc.submitTextCalls != 0 && fc.submitClickCalls != 0 {
		t.Fatalf("SubmitText/SubmitClick called, want neither — a task-done proposal never touches the platform")
	}

	report := campaign.Assemble(campaign.AssembleInput{Goal: g, Campaign: campaignState.Snapshot(), Events: events})
	var navFailures []campaign.Finding
	for _, f := range report.Findings {
		if f.TaskID == "t1" {
			navFailures = append(navFailures, f)
		}
	}
	if len(navFailures) != 1 || navFailures[0].Kind != campaign.FindingAINavigationFailure {
		t.Fatalf("t1 findings = %+v, want exactly one pre-existing ai-navigation-failure", navFailures)
	}
}

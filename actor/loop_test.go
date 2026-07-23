package actor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/goal"
	"chatwright.dev/runtime/observe"
	"chatwright.dev/runtime/platform"
)

// fakeClock is an injectable, manually advanced clock — mirrors goal
// package's own test clock, so duration-budget behaviour is deterministic
// here too.
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func mustCampaign(t *testing.T, g goal.Goal, now func() time.Time) *goal.CampaignState {
	t.Helper()
	campaign, err := goal.NewCampaignState(g, now)
	if err != nil {
		t.Fatalf("NewCampaignState() error = %v", err)
	}
	return campaign
}

func mustLoop(t *testing.T, provider actor.Provider, engine *observe.Engine, actuator actor.Actuator, campaign *goal.CampaignState, g goal.Goal, cfg actor.Config) *actor.Loop {
	t.Helper()
	lp, err := actor.NewLoop(provider, engine, actuator, campaign, g, cfg)
	if err != nil {
		t.Fatalf("NewLoop() error = %v", err)
	}
	return lp
}

const testChatID = int64(1)

var testUser = platform.User{ID: 100, FirstName: "Actor"}

// TestLoopStopsOnEachBudget proves every goal.Budgets dimension the loop is
// responsible for wiring (steps, duration, repeated failures, cost) stops
// the campaign deterministically via the loop, mirroring goal's own
// TestBudgetsProduceDeterministicStopReasons but driven through Loop.RunTask
// instead of calling CampaignState directly.
func TestLoopStopsOnEachBudget(t *testing.T) {
	maxCost := 1.0

	tests := map[string]struct {
		budgets goal.Budgets
		// seed configures fc before the engine is constructed.
		seed func(fc *fakeChat)
		// provider is built from the (already seeded) engine/fc, so it can
		// learn real observe IDs by observing, exactly as any real caller
		// would — never by parsing observe's internal ID format.
		provider func(clock *fakeClock, engine *observe.Engine, fc *fakeChat) actor.Provider
		want     goal.StopReason
	}{
		"steps budget": {
			budgets: goal.Budgets{MaxSteps: 2},
			provider: func(*fakeClock, *observe.Engine, *fakeChat) actor.Provider {
				return actor.NewScriptedProvider(actor.Usage{},
					actor.Proposal{Kind: actor.ProposeSendText, Text: "ping"},
					actor.Proposal{Kind: actor.ProposeSendText, Text: "ping"},
					actor.Proposal{Kind: actor.ProposeSendText, Text: "ping"},
				)
			},
			want: goal.StopBudgetSteps,
		},
		"duration budget": {
			budgets: goal.Budgets{MaxDuration: 5 * time.Minute},
			provider: func(clock *fakeClock, _ *observe.Engine, _ *fakeChat) actor.Provider {
				return actor.ProviderFunc(func(context.Context, actor.Prompt) (actor.Proposal, actor.Usage, error) {
					clock.advance(6 * time.Minute)
					return actor.Proposal{Kind: actor.ProposeSendText, Text: "ping"}, actor.Usage{}, nil
				})
			},
			want: goal.StopBudgetDuration,
		},
		"repeated-failures budget": {
			budgets: goal.Budgets{MaxRepeatedFailures: 2},
			// Two live bot messages, each with its own action. The loop
			// only ever tracks the raw grid of the LATEST one ("Second"),
			// so a proposal that (correctly) validates Fresh against
			// "First"'s still-visible action cannot be resolved to a click
			// target — deterministically ActionResolutionFailed, per Loop's
			// documented single-live-surface scoping limit.
			seed: func(fc *fakeChat) {
				fc.seedBotMessage("First", [][]platform.Action{{{Label: "OldAction", ID: "cb_old"}}})
				fc.seedBotMessage("Second", [][]platform.Action{{{Label: "NewAction", ID: "cb_new"}}})
			},
			provider: func(_ *fakeClock, engine *observe.Engine, _ *fakeChat) actor.Provider {
				pre, err := engine.Observe()
				if err != nil {
					panic(err) // test setup failure, not a scenario under test
				}
				oldActionID := pre.Messages[0].Actions[0].ID
				return actor.NewScriptedProvider(actor.Usage{},
					actor.Proposal{Kind: actor.ProposeClick, ActionID: oldActionID, ObservationSequence: pre.Sequence},
					actor.Proposal{Kind: actor.ProposeClick, ActionID: oldActionID, ObservationSequence: pre.Sequence},
					actor.Proposal{Kind: actor.ProposeClick, ActionID: oldActionID, ObservationSequence: pre.Sequence},
				)
			},
			want: goal.StopRepeatedFailure,
		},
		"cost budget": {
			budgets: goal.Budgets{MaxCost: &maxCost},
			provider: func(*fakeClock, *observe.Engine, *fakeChat) actor.Provider {
				cost := 0.6
				return actor.NewScriptedProvider(actor.Usage{Cost: &cost},
					actor.Proposal{Kind: actor.ProposeSendText, Text: "ping"},
					actor.Proposal{Kind: actor.ProposeSendText, Text: "ping"},
					actor.Proposal{Kind: actor.ProposeSendText, Text: "ping"},
				)
			},
			want: goal.StopBudgetCost,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fc := newFakeChat()
			if tt.seed != nil {
				tt.seed(fc)
			}

			engine := observe.NewEngine(fc, observe.ChatRef{ChatID: testChatID})
			clock := newFakeClock()
			g := goal.Goal{ID: "g", Tasks: []goal.Task{{ID: "t1"}}, Budgets: tt.budgets}
			campaign := mustCampaign(t, g, clock.now)
			provider := tt.provider(clock, engine, fc)
			lp := mustLoop(t, provider, engine, fc, campaign, g, actor.Config{ChatID: testChatID, User: testUser, Now: clock.now})

			result, err := lp.RunTask(context.Background(), "t1")
			if err != nil {
				t.Fatalf("RunTask() error = %v", err)
			}
			if !result.Stopped {
				t.Fatal("campaign did not stop")
			}
			got, ok := campaign.StopReason()
			if !ok || got != tt.want {
				t.Fatalf("StopReason() = %v, %v, want %v, true", got, ok, tt.want)
			}
		})
	}
}

// TestInvalidProposalIsRecordedAndReprompted proves a stale/invalid click
// proposal is recorded as such (never silently mutated or skipped from the
// record) and the loop re-prompts the Provider for the next attempt, which
// succeeds — matching the plan's "Chatwright remains authoritative" design
// decision.
func TestInvalidProposalIsRecordedAndReprompted(t *testing.T) {
	fc := newFakeChat()
	fc.seedBotMessage("Continue?", [][]platform.Action{{{Label: "Yes", ID: "cb_yes"}}})
	fc.queueBotReply("Got it", nil)

	engine := observe.NewEngine(fc, observe.ChatRef{ChatID: testChatID})
	// Learn the real (opaque) action id the same way any caller would: by
	// observing — never by parsing observe's internal ID format.
	pre, err := engine.Observe()
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if len(pre.Messages) != 1 || len(pre.Messages[0].Actions) != 1 {
		t.Fatalf("seed observation = %+v, want one message with one action", pre.Messages)
	}
	realID := pre.Messages[0].Actions[0].ID

	clock := newFakeClock()
	g := goal.Goal{ID: "g", Tasks: []goal.Task{{ID: "t1"}}, Budgets: goal.Budgets{MaxRepeatedFailures: 5}}
	campaign := mustCampaign(t, g, clock.now)
	provider := actor.NewScriptedProvider(actor.Usage{Model: "scripted"},
		actor.Proposal{Kind: actor.ProposeClick, ActionID: "bogus-stale-id", ObservationSequence: pre.Sequence, Rationale: "wrong guess"},
		actor.Proposal{Kind: actor.ProposeClick, ActionID: realID, ObservationSequence: pre.Sequence, Rationale: "the real one"},
		actor.Proposal{Kind: actor.ProposeTaskDone, Rationale: "confirmed"},
	)
	lp := mustLoop(t, provider, engine, fc, campaign, g, actor.Config{ChatID: testChatID, User: testUser, Now: clock.now})

	result, err := lp.RunTask(context.Background(), "t1")
	if err != nil {
		t.Fatalf("RunTask() error = %v", err)
	}
	if result.Status != goal.TaskCompleted {
		t.Fatalf("task status = %v, want completed", result.Status)
	}

	events := lp.Events()
	if len(events) != 3 {
		t.Fatalf("recorded %d events, want 3 (invalid, valid, task-done): %+v", len(events), events)
	}
	if events[0].Action.Kind != actor.ActionSkippedInvalid {
		t.Fatalf("event 0 action = %v, want ActionSkippedInvalid", events[0].Action.Kind)
	}
	if !events[0].Validation.Checked || events[0].Validation.Verdict != observe.VerdictStale {
		t.Fatalf("event 0 validation = %+v, want Checked with VerdictStale", events[0].Validation)
	}
	if events[1].Action.Kind != actor.ActionExecuted {
		t.Fatalf("event 1 action = %v, want ActionExecuted", events[1].Action.Kind)
	}
	if !events[1].Validation.Checked || events[1].Validation.Verdict != observe.VerdictFresh {
		t.Fatalf("event 1 validation = %+v, want Checked with VerdictFresh", events[1].Validation)
	}
	if events[2].Action.Kind != actor.ActionTaskCompleted {
		t.Fatalf("event 2 action = %v, want ActionTaskCompleted", events[2].Action.Kind)
	}

	// The invalid proposal must never have reached the platform: exactly one
	// SubmitClick call (the valid one), never mutated for the bogus one.
	if fc.submitClickCalls != 1 {
		t.Fatalf("SubmitClick called %d times, want exactly 1 (the invalid proposal must not act)", fc.submitClickCalls)
	}
}

// TestNonProgressDetectionStops proves the loop's own non-progress detection
// — N consecutive proposals with no observable bot effect — stops the
// campaign deterministically, independent of any goal.Budgets dimension
// (none is configured here).
func TestNonProgressDetectionStops(t *testing.T) {
	fc := newFakeChat()
	fc.seedBotMessage("Hello", nil) // the bot never queues a reply below: every send is a no-op from its perspective.

	engine := observe.NewEngine(fc, observe.ChatRef{ChatID: testChatID})
	clock := newFakeClock()
	g := goal.Goal{ID: "g", Tasks: []goal.Task{{ID: "t1"}}}
	campaign := mustCampaign(t, g, clock.now)

	script := make([]actor.Proposal, 5)
	for i := range script {
		script[i] = actor.Proposal{Kind: actor.ProposeSendText, Text: "ping"}
	}
	provider := actor.NewScriptedProvider(actor.Usage{}, script...)
	lp := mustLoop(t, provider, engine, fc, campaign, g, actor.Config{ChatID: testChatID, User: testUser, Now: clock.now, NonProgressLimit: 2})

	result, err := lp.RunTask(context.Background(), "t1")
	if err != nil {
		t.Fatalf("RunTask() error = %v", err)
	}
	if !result.NonProgress {
		t.Fatal("result.NonProgress = false, want true")
	}
	if !result.Stopped {
		t.Fatal("result.Stopped = false, want true")
	}
	if !campaign.Stopped() {
		t.Fatal("campaign did not stop")
	}
	if reason, ok := campaign.StopReason(); !ok || reason != goal.StopError {
		t.Fatalf("StopReason() = %v, %v, want StopError, true (non-progress maps to Abort)", reason, ok)
	}

	events := lp.Events()
	if len(events) != 2 {
		t.Fatalf("recorded %d events, want exactly 2 (NonProgressLimit) — the loop must stop itself, not exhaust the 5-entry script", len(events))
	}
	for i, e := range events {
		if e.Action.Kind != actor.ActionExecutedNoEffect {
			t.Fatalf("event %d action = %v, want ActionExecutedNoEffect", i, e.Action.Kind)
		}
	}
	if fc.submitTextCalls != 2 {
		t.Fatalf("SubmitText called %d times, want exactly 2", fc.submitTextCalls)
	}
}

// TestNewLoopRejectsNilClock proves Loop mirrors goal.CampaignState's own
// "no bare time.Now" determinism discipline.
func TestNewLoopRejectsNilClock(t *testing.T) {
	fc := newFakeChat()
	engine := observe.NewEngine(fc, observe.ChatRef{ChatID: testChatID})
	g := goal.Goal{ID: "g", Tasks: []goal.Task{{ID: "t1"}}}
	campaign := mustCampaign(t, g, newFakeClock().now)
	provider := actor.NewScriptedProvider(actor.Usage{})

	_, err := actor.NewLoop(provider, engine, fc, campaign, g, actor.Config{ChatID: testChatID, User: testUser})
	if !errors.Is(err, actor.ErrNilClock) {
		t.Fatalf("NewLoop(nil clock) error = %v, want ErrNilClock", err)
	}
}

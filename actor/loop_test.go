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

// englishActionIn returns the "English" AvailableAction's ID from obs, or
// fails the test — the fixture helper TestNonProgressDetectionSurvives
// IdempotentReEdits' scripted click-provider uses to always target the
// CURRENT observation's action, exactly as a real Provider must (an action's
// synthetic ID changes on every edit — see observe/engine.go's
// availableActionID — so a click test cannot hardcode one upfront).
func englishActionIn(t *testing.T, obs observe.Observation) string {
	t.Helper()
	for _, m := range obs.Messages {
		for _, a := range m.Actions {
			if a.Label == "English" {
				return a.ID
			}
		}
	}
	t.Fatalf("no \"English\" action in observation %+v", obs)
	return ""
}

// TestNonProgressDetectionSurvivesIdempotentReEdits reproduces the model
// arena's non-progress trap (github.com/chatwright/runtime-go/issues/2):
// clicking "English" once genuinely changes the bot's message ("Choose your
// language" -> "Howdy stranger"), but every subsequent click re-edits the
// SAME message with byte-identical text and the same action labels — only
// Version advances. Before the fix, every one of those idempotent re-edits
// still produced an observe.Change (Version bumped) and so was scored as
// ActionExecuted ("progress"), letting a model re-click the same button
// forever without ever tripping NonProgressLimit — exactly what the arena
// run against ollama/qwen3.6:latest did (11 clicks, 0 recorded
// executed-no-effect, budget-steps only stop reason). This test proves the
// fix: the first click counts as progress, and the identical re-edits reset
// nothing, so NonProgressLimit stops the campaign after the Nth
// content-identical re-edit.
func TestNonProgressDetectionSurvivesIdempotentReEdits(t *testing.T) {
	fc := newFakeChat()
	msgID := fc.seedBotMessage("Choose your language", [][]platform.Action{
		{{Label: "English", ID: "cb_en"}},
	})

	// One genuine content change, then three byte-identical re-edits of the
	// same message — the loop must never see the latter three as effects.
	fc.queueBotEdit(msgID, "Howdy stranger", [][]platform.Action{{{Label: "English", ID: "cb_en"}}})
	fc.queueBotEdit(msgID, "Howdy stranger", [][]platform.Action{{{Label: "English", ID: "cb_en"}}})
	fc.queueBotEdit(msgID, "Howdy stranger", [][]platform.Action{{{Label: "English", ID: "cb_en"}}})
	fc.queueBotEdit(msgID, "Howdy stranger", [][]platform.Action{{{Label: "English", ID: "cb_en"}}})

	engine := observe.NewEngine(fc, observe.ChatRef{ChatID: testChatID})
	clock := newFakeClock()
	g := goal.Goal{ID: "g", Tasks: []goal.Task{{ID: "t1"}}}
	campaign := mustCampaign(t, g, clock.now)

	// A Provider that always re-clicks "English" off the CURRENT
	// observation, mirroring a model stuck re-clicking an
	// already-activated button rather than moving on.
	provider := actor.ProviderFunc(func(_ context.Context, p actor.Prompt) (actor.Proposal, actor.Usage, error) {
		return actor.Proposal{
			Kind: actor.ProposeClick, ActionID: englishActionIn(t, p.Observation),
			ObservationSequence: p.Observation.Sequence, Rationale: "pick English",
		}, actor.Usage{}, nil
	})
	lp := mustLoop(t, provider, engine, fc, campaign, g, actor.Config{ChatID: testChatID, User: testUser, Now: clock.now, NonProgressLimit: 3})

	result, err := lp.RunTask(context.Background(), "t1")
	if err != nil {
		t.Fatalf("RunTask() error = %v", err)
	}
	if !result.NonProgress {
		t.Fatal("result.NonProgress = false, want true — the identical re-edits must not be read as progress")
	}
	if !result.Stopped {
		t.Fatal("result.Stopped = false, want true")
	}

	events := lp.Events()
	if len(events) != 4 {
		t.Fatalf("recorded %d events, want exactly 4 (1 genuine click + NonProgressLimit=3 identical re-edits): %+v", len(events), events)
	}
	if events[0].Action.Kind != actor.ActionExecuted {
		t.Fatalf("event 0 (the genuine content change) action = %v, want ActionExecuted", events[0].Action.Kind)
	}
	for i := 1; i < 4; i++ {
		if events[i].Action.Kind != actor.ActionExecutedNoEffect {
			t.Fatalf("event %d (an identical re-edit) action = %v, want ActionExecutedNoEffect", i, events[i].Action.Kind)
		}
	}
	if fc.submitClickCalls != 4 {
		t.Fatalf("SubmitClick called %d times, want exactly 4", fc.submitClickCalls)
	}
}

// TestGenuineEditsStillCountAsProgress proves the fix does not overcorrect:
// a message edited TWICE in a row, with genuinely different text and
// actions each time (never a byte-identical re-render), counts as progress
// both times — so the fix only suppresses content-identical re-renders,
// never real ones.
func TestGenuineEditsStillCountAsProgress(t *testing.T) {
	fc := newFakeChat()
	msgID := fc.seedBotMessage("Choose your language", [][]platform.Action{
		{{Label: "English", ID: "cb_en"}},
	})
	// Two distinct, real edits of the same message: text and the action set
	// both genuinely change each time.
	fc.queueBotEdit(msgID, "Howdy stranger", [][]platform.Action{{{Label: "Continue", ID: "cb_continue"}}})
	fc.queueBotEdit(msgID, "All set!", nil)

	engine := observe.NewEngine(fc, observe.ChatRef{ChatID: testChatID})
	clock := newFakeClock()
	g := goal.Goal{ID: "g", Tasks: []goal.Task{{ID: "t1"}}}
	campaign := mustCampaign(t, g, clock.now)

	// Always click whichever action the CURRENT observation shows, once one
	// remains, moving to task-done: real IDs change on every edit (see
	// observe/engine.go's availableActionID), so a fixed ScriptedProvider
	// script cannot drive this across two edits the way it can drive a
	// single click (see the repeated-failures budget case in
	// TestLoopStopsOnEachBudget).
	provider := actor.ProviderFunc(func(_ context.Context, p actor.Prompt) (actor.Proposal, actor.Usage, error) {
		for _, m := range p.Observation.Messages {
			for _, a := range m.Actions {
				return actor.Proposal{
					Kind: actor.ProposeClick, ActionID: a.ID,
					ObservationSequence: p.Observation.Sequence, Rationale: "advance",
				}, actor.Usage{}, nil
			}
		}
		return actor.Proposal{Kind: actor.ProposeTaskDone, Rationale: "no actions left"}, actor.Usage{}, nil
	})
	lp := mustLoop(t, provider, engine, fc, campaign, g, actor.Config{ChatID: testChatID, User: testUser, Now: clock.now, NonProgressLimit: 2})

	result, err := lp.RunTask(context.Background(), "t1")
	if err != nil {
		t.Fatalf("RunTask() error = %v", err)
	}
	if result.NonProgress {
		t.Fatal("result.NonProgress = true, want false — two genuine content changes must both count as progress")
	}
	if result.Status != goal.TaskCompleted {
		t.Fatalf("task status = %v, want completed", result.Status)
	}

	events := lp.Events()
	if len(events) != 3 {
		t.Fatalf("recorded %d events, want exactly 3 (2 genuine edits + task-done): %+v", len(events), events)
	}
	for i := 0; i < 2; i++ {
		if events[i].Action.Kind != actor.ActionExecuted {
			t.Fatalf("event %d action = %v, want ActionExecuted", i, events[i].Action.Kind)
		}
	}
	if events[2].Action.Kind != actor.ActionTaskCompleted {
		t.Fatalf("event 2 action = %v, want ActionTaskCompleted", events[2].Action.Kind)
	}
}

// TestProposeErrorLeavesLoopEventEvidence reproduces
// github.com/chatwright/runtime-go/issue #4: a Provider.Propose error used
// to abort RunTask (a returned Go error, unchanged by this test) while
// leaving absolutely no trace in Loop.Events — a run bundle assembled from
// that Loop would show a campaign that just stopped, with nothing
// explaining why. This proves the fix: the failing iteration still gets a
// LoopEvent (index, timestamp, task, the observation it was attempting to
// act from, and the error captured in ProposeError), recorded BEFORE
// RunTask returns, alongside the normal event from the call before it —
// while RunTask's own abort-via-returned-error behaviour, and the
// campaign's own stopped state, are both otherwise unchanged.
func TestProposeErrorLeavesLoopEventEvidence(t *testing.T) {
	fc := newFakeChat()
	fc.seedBotMessage("Hello", nil)
	fc.queueBotReply("pong", nil) // consumed by the one successful call only

	engine := observe.NewEngine(fc, observe.ChatRef{ChatID: testChatID})
	clock := newFakeClock()
	g := goal.Goal{ID: "g", Tasks: []goal.Task{{ID: "t1"}}}
	campaign := mustCampaign(t, g, clock.now)

	errPropose := errors.New("actor/fake: transport failed: context deadline exceeded")
	calls := 0
	provider := actor.ProviderFunc(func(context.Context, actor.Prompt) (actor.Proposal, actor.Usage, error) {
		calls++
		if calls == 2 {
			return actor.Proposal{}, actor.Usage{}, errPropose
		}
		return actor.Proposal{Kind: actor.ProposeSendText, Text: "ping"}, actor.Usage{Model: "scripted"}, nil
	})
	lp := mustLoop(t, provider, engine, fc, campaign, g, actor.Config{ChatID: testChatID, User: testUser, Now: clock.now})

	result, err := lp.RunTask(context.Background(), "t1")
	if err == nil {
		t.Fatal("RunTask() error = nil, want the Propose error to abort the task — existing behaviour, unchanged")
	}
	if !errors.Is(err, errPropose) {
		t.Fatalf("RunTask() error = %v, want it to wrap the Provider's error", err)
	}
	if result != (actor.TaskResult{}) {
		t.Fatalf("RunTask() result = %+v, want the zero TaskResult on error (unchanged)", result)
	}
	if campaign.Stopped() {
		t.Fatal("campaign.Stopped() = true, want false — a Propose error aborts this RunTask call via its returned error; it does not itself stop the campaign (unchanged behaviour)")
	}

	events := lp.Events()
	if len(events) != 2 {
		t.Fatalf("recorded %d events, want exactly 2 (1 normal + 1 propose-error): %+v", len(events), events)
	}

	first := events[0]
	if first.Index != 0 {
		t.Fatalf("event 0 Index = %d, want 0", first.Index)
	}
	if first.ProposeError != "" {
		t.Fatalf("event 0 ProposeError = %q, want empty", first.ProposeError)
	}
	if first.Proposal.Kind != actor.ProposeSendText {
		t.Fatalf("event 0 Proposal.Kind = %v, want ProposeSendText", first.Proposal.Kind)
	}

	second := events[1]
	if second.Index != 1 {
		t.Fatalf("event 1 Index = %d, want 1", second.Index)
	}
	if second.TaskID != "t1" {
		t.Fatalf("event 1 TaskID = %q, want \"t1\"", second.TaskID)
	}
	if second.At.IsZero() {
		t.Fatal("event 1 At is zero, want the loop's clock reading")
	}
	if second.ObservationSequence == 0 {
		t.Fatal("event 1 ObservationSequence = 0, want the observation this iteration observed before proposing")
	}
	if second.ProposeError != errPropose.Error() {
		t.Fatalf("event 1 ProposeError = %q, want %q", second.ProposeError, errPropose.Error())
	}
	if second.Proposal != (actor.Proposal{}) {
		t.Fatalf("event 1 Proposal = %+v, want the zero value — no proposal was ever produced", second.Proposal)
	}
	if second.Usage != (actor.Usage{}) {
		t.Fatalf("event 1 Usage = %+v, want the zero value", second.Usage)
	}
	if second.Validation != (actor.ValidationOutcome{}) {
		t.Fatalf("event 1 Validation = %+v, want the zero value", second.Validation)
	}
	if second.Action != (actor.ActionOutcome{}) {
		t.Fatalf("event 1 Action = %+v, want the zero value — no action was ever taken", second.Action)
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

package run_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/cw"
	"chatwright.dev/runtime/goal"
	"chatwright.dev/runtime/platform"
	"chatwright.dev/runtime/run"
	"chatwright.dev/sdk"
)

// fakeClock is an injectable, manually advanced clock — mirrors the same
// pattern goal/campaign_test.go and actor/loop_test.go each keep their own
// copy of, so duration-budget behaviour is deterministic here too.
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) now() time.Time { return c.t }

// fakeEmulator is a minimal, fully controlled stand-in for a live
// platform.Emulator, implementing the whole interface over one shared,
// in-memory, per-chat conversation — the same purpose actor/fakechat_test.go's
// unexported fakeChat serves for actor's own unit tests (that type cannot be
// reused across packages, hence this package's own copy). Bot behaviour is
// scripted with queueBotReply, consumed FIFO by the next SubmitText/
// SubmitClick call on the same chat. The real Telegram emulator is exercised
// separately, in run_e2e_test.go.
type fakeEmulator struct {
	mu sync.Mutex

	now func() time.Time

	nextMsgID map[int64]int
	entries   map[int64][]platform.JournalEntry
	messages  map[int64][]platform.Message
	queue     map[int64][]func()
}

func newFakeEmulator(now func() time.Time) *fakeEmulator {
	return &fakeEmulator{
		now:       now,
		nextMsgID: make(map[int64]int),
		entries:   make(map[int64][]platform.JournalEntry),
		messages:  make(map[int64][]platform.Message),
		queue:     make(map[int64][]func()),
	}
}

func (f *fakeEmulator) BotAPIURL() string               { return "" }
func (f *fakeEmulator) Close()                          {}
func (f *fakeEmulator) SetWebhook(string, *http.Client) {}
func (f *fakeEmulator) Transcript(int64) string         { return "" }
func (f *fakeEmulator) WaitForEdit(int64, int, int, time.Duration) (*platform.Message, bool) {
	return nil, false // no fakeEmulator-backed unit test exercises an edit.
}

func (f *fakeEmulator) reserveMsgIDLocked(chatID int64) int {
	f.nextMsgID[chatID]++
	return f.nextMsgID[chatID]
}

// queueBotReply schedules the next SubmitText/SubmitClick call on chatID to
// also produce a new bot message.
func (f *fakeEmulator) queueBotReply(chatID int64, text string, actions [][]platform.Action) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue[chatID] = append(f.queue[chatID], func() {
		id := f.reserveMsgIDLocked(chatID)
		f.appendBotMessageLocked(chatID, id, text, actions)
	})
}

func (f *fakeEmulator) appendBotMessageLocked(chatID int64, id int, text string, actions [][]platform.Action) {
	f.entries[chatID] = append(f.entries[chatID], platform.JournalEntry{
		Direction: platform.DirectionBot, Kind: platform.JournalEntryMessage,
		MessageID: id, Text: text, Actions: actions, At: f.now(), FromID: fakeBotUserID,
	})
	f.messages[chatID] = append(f.messages[chatID], platform.Message{
		Platform: "fake", ChatID: chatID, MessageID: id, Text: text, Actions: actions, ReceivedAt: f.now(),
	})
}

// fakeBotUserID is the fixed FromID fakeEmulator stamps on every bot-authored
// entry — mirrors telegram.EmulatedBotUserID's role for the real emulator.
const fakeBotUserID = int64(1)

func (f *fakeEmulator) SubmitText(chatID int64, user platform.User, text string) error {
	f.mu.Lock()
	id := f.reserveMsgIDLocked(chatID)
	f.entries[chatID] = append(f.entries[chatID], platform.JournalEntry{
		Direction: platform.DirectionUser, Kind: platform.JournalEntryMessage,
		MessageID: id, Text: text, At: f.now(), FromID: user.ID,
	})
	f.consumeQueueLocked(chatID)
	f.mu.Unlock()
	return nil
}

func (f *fakeEmulator) SubmitClick(chatID int64, user platform.User, data string, targetMessageID int) error {
	f.mu.Lock()
	f.entries[chatID] = append(f.entries[chatID], platform.JournalEntry{
		Direction: platform.DirectionUser, Kind: platform.JournalEntryAction,
		RefMessageID: targetMessageID, Text: data, At: f.now(), FromID: user.ID,
	})
	f.consumeQueueLocked(chatID)
	f.mu.Unlock()
	return nil
}

func (f *fakeEmulator) consumeQueueLocked(chatID int64) {
	q := f.queue[chatID]
	if len(q) == 0 {
		return
	}
	next := q[0]
	f.queue[chatID] = q[1:]
	next()
}

func (f *fakeEmulator) WaitForMessage(chatID int64, consumed int, _ time.Duration) (*platform.Message, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	msgs := f.messages[chatID]
	if consumed < 0 || consumed >= len(msgs) {
		return nil, false
	}
	msg := msgs[consumed]
	return &msg, true
}

func (f *fakeEmulator) Journal(chatID int64) ([]platform.JournalEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]platform.JournalEntry(nil), f.entries[chatID]...), nil
}

// --- TestAIGoalThenDeterministicHandover -----------------------------------

// onboardingFollowUpInput is the trivial input type for the deterministic
// part's fragment below — it needs nothing from its caller, only the shared
// emulator/chat identity closed over from the test itself.
type onboardingFollowUpInput struct{}

// TestAIGoalThenDeterministicHandover proves the composition contract
// supports a deterministic Part AFTER an ai-goal Part — the direction
// spec/ideas/hybrid-runs.md's MVP scope explicitly calls out as
// API-supported-but-not-in-the-founder's-proof-scenario ("Deterministic
// parts after an AI part ... remain supported by the composition API and
// unit-tested, but are not part of the MVP proof scenario"): the
// deterministic fragment can see, in the shared journal, exactly what the
// preceding ai-goal Part produced, and can go on to act itself.
func TestAIGoalThenDeterministicHandover(t *testing.T) {
	const chatID = int64(1)
	user := platform.User{ID: 9, FirstName: "Explorer"}
	clock := newFakeClock()
	emu := newFakeEmulator(clock.now)
	emu.queueBotReply(chatID, "ai-ack", nil) // gives the AI part's send-text an observable effect.

	aiGoal := goal.Goal{
		ID: "announce", Title: "Announce readiness",
		Tasks: []goal.Task{{ID: "announce", SuccessCriteria: "a message is sent"}},
	}
	provider := actor.NewScriptedProvider(actor.Usage{Model: "scripted-v1"},
		actor.Proposal{Kind: actor.ProposeSendText, Text: "hello-from-ai", Rationale: "announce"},
		actor.Proposal{Kind: actor.ProposeTaskDone, Rationale: "announced"},
	)
	aiPart := run.NewAIGoalPart("ai-announce", "AI announcement", "", run.AIGoalPartInput{
		ActorID: "explorer", Goal: aiGoal, Provider: provider,
		Config: actor.Config{ChatID: chatID, User: user},
	})

	sawHandover := false
	source := cw.SourceReference{URI: "https://example.test/onboarding-followup.go", Revision: "abc123"}
	fragment := cw.Fragment[onboardingFollowUpInput]{
		Definition:  cw.Definition{Name: "onboarding-followup", Source: source},
		CloneInputs: func(in onboardingFollowUpInput) onboardingFollowUpInput { return in },
		Execute: func(ec *cw.ExecutionContext, _ onboardingFollowUpInput) error {
			entries, err := emu.Journal(chatID)
			if err != nil {
				return err
			}
			for _, e := range entries {
				if e.Direction == platform.DirectionUser && e.Text == "hello-from-ai" {
					sawHandover = true
				}
			}
			if !sawHandover {
				return errors.New("deterministic followup did not see the AI part's own message in the shared journal")
			}
			ec.RecordStep("confirmed the AI part's message is visible", source)
			return emu.SubmitText(chatID, user, "deterministic-followup")
		},
	}
	detPart := run.NewDeterministicPart("deterministic-followup", "Deterministic follow-up", "", fragment, cw.EffectiveInputs[onboardingFollowUpInput]{})

	r := run.Run{
		ID:          "ai-then-deterministic",
		Environment: run.Environment{Emulator: emu, ChatIDs: []int64{chatID}, Now: clock.now},
		Parts:       []run.Part{aiPart, detPart},
	}

	result, err := r.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !sawHandover {
		t.Fatal("deterministic part's fragment never observed the AI part's own message")
	}
	if len(result.Parts) != 2 {
		t.Fatalf("len(result.Parts) = %d, want 2: %+v", len(result.Parts), result.Parts)
	}
	if len(result.Skipped) != 0 {
		t.Fatalf("result.Skipped = %+v, want none", result.Skipped)
	}

	aiOutcome, detOutcome := result.Parts[0], result.Parts[1]
	if aiOutcome.PartID != "ai-announce" || aiOutcome.Status != run.PartCompleted {
		t.Fatalf("ai outcome = %+v, want completed ai-announce", aiOutcome)
	}
	if detOutcome.PartID != "deterministic-followup" || detOutcome.Status != run.PartCompleted {
		t.Fatalf("deterministic outcome = %+v, want completed deterministic-followup", detOutcome)
	}
	if detOutcome.Err != nil {
		t.Fatalf("deterministic outcome.Err = %v, want nil", detOutcome.Err)
	}

	// Provenance retained: the fragment's own RecordStep call shows up in
	// the outcome, not silently dropped.
	if detOutcome.Deterministic == nil || len(detOutcome.Deterministic.Steps) != 1 {
		t.Fatalf("detOutcome.Deterministic = %+v, want exactly one retained step", detOutcome.Deterministic)
	}
	if detOutcome.Deterministic.Definition.Name != "onboarding-followup" {
		t.Fatalf("detOutcome.Deterministic.Definition = %+v, want name onboarding-followup", detOutcome.Deterministic.Definition)
	}

	// The two boundaries are adjacent and non-overlapping over the one
	// shared chat, covering the whole journal between them.
	if len(aiOutcome.Boundary.Chats) != 1 || len(detOutcome.Boundary.Chats) != 1 {
		t.Fatalf("boundaries = ai:%+v det:%+v, want exactly one chat boundary each", aiOutcome.Boundary, detOutcome.Boundary)
	}
	aiChat, detChat := aiOutcome.Boundary.Chats[0], detOutcome.Boundary.Chats[0]
	if aiChat.FirstEntry != 0 {
		t.Fatalf("ai part boundary FirstEntry = %d, want 0", aiChat.FirstEntry)
	}
	if detChat.FirstEntry != aiChat.FirstEntry+aiChat.EntryCount {
		t.Fatalf("deterministic part boundary FirstEntry = %d, want %d (adjacent to the AI part's end)", detChat.FirstEntry, aiChat.FirstEntry+aiChat.EntryCount)
	}
	entries, err := emu.Journal(chatID)
	if err != nil {
		t.Fatalf("Journal() error = %v", err)
	}
	if got, want := detChat.FirstEntry+detChat.EntryCount, len(entries); got != want {
		t.Fatalf("combined boundaries cover %d entries, want %d (the whole journal)", got, want)
	}
}

// --- TestRunCeilingAttributesReasonAndPart ---------------------------------

// TestRunCeilingAttributesReasonAndPart proves a run.RunCeiling trips
// deterministically mid-Part (between two tasks of the same ai-goal Part)
// and the resulting run.CeilingTrip names both the aggregate reason and the
// Part it tripped in — spec/ideas/hybrid-runs.md's "when the ceiling trips
// mid-part, the stop reason must attribute both the run ceiling and the
// part it tripped in" — and that every subsequent declared Part is aborted
// (never executed), regardless of its own FailurePolicy.
func TestRunCeilingAttributesReasonAndPart(t *testing.T) {
	const chatID = int64(1)
	user := platform.User{ID: 9, FirstName: "Explorer"}
	clock := newFakeClock()
	emu := newFakeEmulator(clock.now)

	g := goal.Goal{
		ID: "two-tasks",
		Tasks: []goal.Task{
			{ID: "t1", SuccessCriteria: "first task done"},
			{ID: "t2", SuccessCriteria: "second task done"},
		},
	}
	provider := actor.NewScriptedProvider(actor.Usage{},
		actor.Proposal{Kind: actor.ProposeTaskDone, Rationale: "t1 done"},
		actor.Proposal{Kind: actor.ProposeTaskDone, Rationale: "t2 done"},
	)
	aiPart := run.NewAIGoalPart("two-task-part", "", "", run.AIGoalPartInput{
		ActorID: "explorer", Goal: g, Provider: provider,
		Config: actor.Config{ChatID: chatID, User: user},
	})

	secondPartRan := false
	fragment := cw.Fragment[struct{}]{
		Definition:  cw.Definition{Name: "never-reached"},
		CloneInputs: func(in struct{}) struct{} { return in },
		Execute: func(*cw.ExecutionContext, struct{}) error {
			secondPartRan = true
			return nil
		},
	}
	secondPart := run.NewDeterministicPart("never-reached", "", "", fragment, cw.EffectiveInputs[struct{}]{})

	r := run.Run{
		ID:          "ceiling-trip",
		Environment: run.Environment{Emulator: emu, ChatIDs: []int64{chatID}, Now: clock.now},
		Parts:       []run.Part{aiPart, secondPart},
		Ceiling:     run.RunCeiling{MaxSteps: 1},
	}

	result, err := r.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if secondPartRan {
		t.Fatal("the second part ran despite the run ceiling tripping in the first")
	}
	if len(result.Parts) != 1 {
		t.Fatalf("len(result.Parts) = %d, want 1: %+v", len(result.Parts), result.Parts)
	}

	outcome := result.Parts[0]
	if outcome.Status != run.PartCeilingStopped {
		t.Fatalf("outcome.Status = %v, want PartCeilingStopped", outcome.Status)
	}
	if outcome.CeilingTrip == nil {
		t.Fatal("outcome.CeilingTrip = nil, want a trip attributed to this part")
	}
	if outcome.CeilingTrip.Reason != goal.StopBudgetSteps {
		t.Fatalf("outcome.CeilingTrip.Reason = %v, want %v", outcome.CeilingTrip.Reason, goal.StopBudgetSteps)
	}
	if outcome.CeilingTrip.PartID != "two-task-part" {
		t.Fatalf("outcome.CeilingTrip.PartID = %q, want %q", outcome.CeilingTrip.PartID, "two-task-part")
	}
	if result.CeilingTrip == nil || *result.CeilingTrip != *outcome.CeilingTrip {
		t.Fatalf("result.CeilingTrip = %+v, want it to mirror the tripping part's own trip %+v", result.CeilingTrip, outcome.CeilingTrip)
	}

	// The interrupted part's own report shows exactly what really
	// happened: t1 completed, t2 never attempted.
	if outcome.AIGoal == nil {
		t.Fatal("outcome.AIGoal = nil, want the partial evidence for the interrupted part")
	}
	statuses := make(map[string]string, len(outcome.AIGoal.Report.Tasks))
	for _, task := range outcome.AIGoal.Report.Tasks {
		statuses[task.TaskID] = task.Status
	}
	if statuses["t1"] != string(goal.TaskCompleted) {
		t.Fatalf("t1 status = %q, want completed", statuses["t1"])
	}
	if statuses["t2"] != string(goal.TaskPending) {
		t.Fatalf("t2 status = %q, want pending (never attempted before the ceiling tripped)", statuses["t2"])
	}

	if len(result.Skipped) != 1 || result.Skipped[0].PartID != "never-reached" || result.Skipped[0].Status != run.PartAborted {
		t.Fatalf("result.Skipped = %+v, want exactly one aborted never-reached entry", result.Skipped)
	}
}

// --- Failure-policy behaviour ------------------------------------------

// buildFailingDeterministicPart returns a deterministic Part whose fragment
// always fails with wantErr, plus a Part guaranteed to record whether it
// ever ran — used by both failure-policy tests below.
func buildFailingDeterministicPart(id string, policy run.FailurePolicy, wantErr error) run.Part {
	fragment := cw.Fragment[struct{}]{
		Definition:  cw.Definition{Name: id},
		CloneInputs: func(in struct{}) struct{} { return in },
		Execute:     func(*cw.ExecutionContext, struct{}) error { return wantErr },
	}
	return run.NewDeterministicPart(id, "", policy, fragment, cw.EffectiveInputs[struct{}]{})
}

func buildTrackingDeterministicPart(id string, ran *bool) run.Part {
	fragment := cw.Fragment[struct{}]{
		Definition:  cw.Definition{Name: id},
		CloneInputs: func(in struct{}) struct{} { return in },
		Execute: func(*cw.ExecutionContext, struct{}) error {
			*ran = true
			return nil
		},
	}
	return run.NewDeterministicPart(id, "", "", fragment, cw.EffectiveInputs[struct{}]{})
}

// TestFailurePolicyAbortStopsRun proves the zero-value FailurePolicy ("")
// behaves exactly like the explicit FailurePolicyAbort — see FailurePolicy's
// own doc comment on why that is the documented default rather than a
// silent one: a failed Part halts the whole Run, and the next declared Part
// never executes and is not marked a coverage gap either.
func TestFailurePolicyAbortStopsRun(t *testing.T) {
	const chatID = int64(1)
	clock := newFakeClock()
	emu := newFakeEmulator(clock.now)
	wantErr := errors.New("onboarding fragment failed")

	var secondRan bool
	failing := buildFailingDeterministicPart("failing-part", "", wantErr) // "" is the zero value: exercises the documented abort default.
	second := buildTrackingDeterministicPart("second-part", &secondRan)

	r := run.Run{
		ID:          "abort-policy",
		Environment: run.Environment{Emulator: emu, ChatIDs: []int64{chatID}, Now: clock.now},
		Parts:       []run.Part{failing, second},
	}

	result, err := r.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if secondRan {
		t.Fatal("the second part ran despite the first part aborting under the (default) abort policy")
	}
	if len(result.Parts) != 1 || result.Parts[0].Status != run.PartFailed || !errors.Is(result.Parts[0].Err, wantErr) {
		t.Fatalf("result.Parts = %+v, want exactly one failed part wrapping %v", result.Parts, wantErr)
	}
	if len(result.Skipped) != 1 || result.Skipped[0].PartID != "second-part" || result.Skipped[0].Status != run.PartAborted {
		t.Fatalf("result.Skipped = %+v, want exactly one aborted second-part entry", result.Skipped)
	}
}

// TestFailurePolicyCoverageGapMarksSubsequentParts proves
// FailurePolicyCoverageGap lets a Run continue past a failed deterministic
// Part while marking every subsequent Part a coverage gap instead of
// executing it — the opt-in alternative to FailurePolicyAbort.
func TestFailurePolicyCoverageGapMarksSubsequentParts(t *testing.T) {
	const chatID = int64(1)
	clock := newFakeClock()
	emu := newFakeEmulator(clock.now)
	wantErr := errors.New("onboarding fragment failed")

	var secondRan, thirdRan bool
	failing := buildFailingDeterministicPart("failing-part", run.FailurePolicyCoverageGap, wantErr)
	second := buildTrackingDeterministicPart("second-part", &secondRan)
	third := buildTrackingDeterministicPart("third-part", &thirdRan)

	r := run.Run{
		ID:          "coverage-gap-policy",
		Environment: run.Environment{Emulator: emu, ChatIDs: []int64{chatID}, Now: clock.now},
		Parts:       []run.Part{failing, second, third},
	}

	result, err := r.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if secondRan || thirdRan {
		t.Fatal("a subsequent part ran despite the coverage-gap policy")
	}
	if len(result.Parts) != 1 || result.Parts[0].Status != run.PartFailed {
		t.Fatalf("result.Parts = %+v, want exactly one failed part", result.Parts)
	}
	if len(result.Skipped) != 2 {
		t.Fatalf("result.Skipped = %+v, want two coverage-gap entries", result.Skipped)
	}
	for _, skipped := range result.Skipped {
		if skipped.Status != run.PartCoverageGap {
			t.Fatalf("skipped part %q status = %v, want PartCoverageGap", skipped.PartID, skipped.Status)
		}
	}
	if result.Skipped[0].PartID != "second-part" || result.Skipped[1].PartID != "third-part" {
		t.Fatalf("result.Skipped = %+v, want [second-part, third-part] in order", result.Skipped)
	}
}

// TestAssembleBundleRunOmitsSkippedParts proves AssembleBundleRun only turns
// executed Parts into sdk.Part entries — a Part the Run never reached has
// no journal evidence to bound and is therefore absent from the persisted
// sdk.Run, per AssembleBundleRun's own doc comment.
func TestAssembleBundleRunOmitsSkippedParts(t *testing.T) {
	const chatID = int64(1)
	clock := newFakeClock()
	emu := newFakeEmulator(clock.now)
	wantErr := errors.New("boom")

	var secondRan bool
	failing := buildFailingDeterministicPart("failing-part", run.FailurePolicyCoverageGap, wantErr)
	second := buildTrackingDeterministicPart("second-part", &secondRan)

	r := run.Run{
		ID:          "assemble-omits-skipped",
		Environment: run.Environment{Emulator: emu, ChatIDs: []int64{chatID}, Now: clock.now},
		Parts:       []run.Part{failing, second},
	}
	result, err := r.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	bundleRun := run.AssembleBundleRun(run.AssembleBundleRunInput{
		RunID: "assemble-omits-skipped", Platform: "fake", EndpointProfile: sdk.EndpointProfilePlatformEmulated,
		Chats:  []sdk.ChatJournal{{ChatID: chatID}},
		Result: result,
	})
	if len(bundleRun.Parts) != 1 || bundleRun.Parts[0].ID != "failing-part" {
		t.Fatalf("bundleRun.Parts = %+v, want exactly one part (failing-part)", bundleRun.Parts)
	}
}

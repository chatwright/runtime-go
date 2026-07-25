package scenario_test

// TestGreetbotDocumentReproducesArenaScenario is the feature's named proof
// (AGENTS.md principle 6, "each feature proves its existence"; the
// self-contained-scenario-documents README's greetbot-scenario-is-
// expressible acceptance criterion): the worked-example greetbot document
// — committed verbatim at testdata/greetbot-language-onboarding.json —
// parses, validates and builds into a run.Run whose resolved identity,
// goal, budgets, roster and journal-verification verdict are the same as
// arena.GreetbotScenario()'s, with no Go or TypeScript rebuilt to run it:
// this test only ever reads the JSON fixture, never
// arena.GreetbotScenario()'s own Go definition, to build the document side
// of every comparison.
import (
	"context"
	"reflect"
	"testing"
	"time"

	"chatwright.dev/runtime/arena"
	"chatwright.dev/runtime/run"
	"chatwright.dev/runtime/scenario"
	"chatwright.dev/sdk"
)

func TestGreetbotDocumentReproducesArenaScenario(t *testing.T) {
	ctx := context.Background()

	provider := scenario.FileScenarioProvider{}
	doc, report, err := provider.Load(ctx, "testdata/greetbot-language-onboarding.json")
	if err != nil {
		t.Fatalf("Load() error = %v\nreport = %+v", err, report)
	}
	for _, w := range report.Warnings() {
		t.Logf("report warning: %+v", w)
	}

	reference := arena.GreetbotScenario()

	// --- identity: scenario id, version, title ---
	if doc.ID != reference.ID {
		t.Errorf("doc.ID = %q, want %q (arena.GreetbotScenario().ID)", doc.ID, reference.ID)
	}
	if doc.Version != reference.Version {
		t.Errorf("doc.Version = %q, want %q", doc.Version, reference.Version)
	}
	if doc.Title != reference.Title {
		t.Errorf("doc.Title = %q, want %q", doc.Title, reference.Title)
	}

	// --- goal: id, task id, success criteria (byte-identical), budgets ---
	if len(doc.Parts) != 1 {
		t.Fatalf("len(doc.Parts) = %d, want 1 (the same single ordered part as arena.GreetbotScenario())", len(doc.Parts))
	}
	docGoal := doc.Parts[0].Goal
	if docGoal == nil {
		t.Fatal("doc.Parts[0].Goal is nil")
	}
	if docGoal.ID != reference.Goal.ID {
		t.Errorf("goal.ID = %q, want %q", docGoal.ID, reference.Goal.ID)
	}
	if docGoal.Title != reference.Goal.Title {
		t.Errorf("goal.Title = %q, want %q", docGoal.Title, reference.Goal.Title)
	}
	if len(docGoal.Tasks) != 1 || len(reference.Goal.Tasks) != 1 {
		t.Fatalf("task counts differ: doc=%d reference=%d, want 1 each", len(docGoal.Tasks), len(reference.Goal.Tasks))
	}
	if docGoal.Tasks[0].ID != reference.Goal.Tasks[0].ID {
		t.Errorf("task.ID = %q, want %q", docGoal.Tasks[0].ID, reference.Goal.Tasks[0].ID)
	}
	if docGoal.Tasks[0].SuccessCriteria != reference.Goal.Tasks[0].SuccessCriteria {
		t.Errorf("task.SuccessCriteria is not byte-identical to arena.GreetbotScenario()'s Go string:\n got:  %q\nwant: %q",
			docGoal.Tasks[0].SuccessCriteria, reference.Goal.Tasks[0].SuccessCriteria)
	}
	if docGoal.Budgets.MaxSteps != reference.Goal.Budgets.MaxSteps {
		t.Errorf("budgets.MaxSteps = %d, want %d", docGoal.Budgets.MaxSteps, reference.Goal.Budgets.MaxSteps)
	}
	gotDuration := time.Duration(docGoal.Budgets.MaxDurationSeconds) * time.Second
	if gotDuration != reference.Goal.Budgets.MaxDuration {
		t.Errorf("budgets.MaxDurationSeconds resolves to %v, want %v", gotDuration, reference.Goal.Budgets.MaxDuration)
	}

	// --- the same chat, user and bot roster identity as
	// arena.GreetbotScenario()'s own Setup() produces ---
	refSession, err := reference.Setup()
	if err != nil {
		t.Fatalf("reference.Setup() error = %v", err)
	}
	defer refSession.Close()

	if len(doc.Chats) != 1 {
		t.Fatalf("len(doc.Chats) = %d, want 1", len(doc.Chats))
	}
	if doc.Chats[0].PlatformChatID != refSession.ChatID {
		t.Errorf("chat id = %d, want %d (arena's own chatID)", doc.Chats[0].PlatformChatID, refSession.ChatID)
	}
	if len(doc.Cast) != 1 {
		t.Fatalf("len(doc.Cast) = %d, want 1", len(doc.Cast))
	}
	castIdentity := doc.Cast[0].PlatformIdentity
	if castIdentity.UserID != refSession.User.ID || castIdentity.FirstName != refSession.User.FirstName {
		t.Errorf("cast[0].platformIdentity = %+v, want {UserID:%d FirstName:%q} (arena's own user)",
			castIdentity, refSession.User.ID, refSession.User.FirstName)
	}

	// --- build and execute the document itself ---
	// BuildOptions.Now is deliberately left unset: this greetbot document's
	// only cast member is a cassette-replay provider, so Build's own
	// defaultClock freezes the clock automatically — the same default a
	// bare `chatwright run` gets — proving the shipped fixture replays
	// deterministically with no caller-supplied clock at all.
	built, err := scenario.Build(ctx, doc, scenario.BuildOptions{})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	defer built.Close()

	// The bot roster identity Build produced must equal arena's own
	// hardcoded BotActor exactly — same ID, Type, Name and platform
	// identity (telegram.EmulatedBotUserID, assigned by the emulator, never
	// authored — see the README's "the bot's platform identity is assigned
	// by the emulator, not the document").
	var botActor *sdk.Actor
	for i := range built.Actors {
		if built.Actors[i].Type == sdk.ActorBot {
			botActor = &built.Actors[i]
		}
	}
	if botActor == nil {
		t.Fatal("built.Actors has no ActorBot entry")
	}
	if !reflect.DeepEqual(*botActor, refSession.BotActor) {
		t.Errorf("bot roster actor =\n  %+v\nwant (arena's own BotActor):\n  %+v", *botActor, refSession.BotActor)
	}

	result, err := built.Run.Execute(ctx)
	if err != nil {
		t.Fatalf("Run.Execute() error = %v", err)
	}
	if len(result.Parts) != 1 {
		t.Fatalf("len(result.Parts) = %d, want 1 (the same ordered single part)", len(result.Parts))
	}
	if result.Parts[0].Status != run.PartCompleted {
		t.Fatalf("result.Parts[0].Status = %v, want PartCompleted", result.Parts[0].Status)
	}
	if result.Parts[0].Kind != sdk.PartKindAIGoal {
		t.Fatalf("result.Parts[0].Kind = %v, want PartKindAIGoal", result.Parts[0].Kind)
	}

	// --- the journal verification returns the same verdict and the same
	// detail string as arena.GreetbotScenario().Verify (verifyGreetbotJournal)
	// for this run's own journal ---
	platformChatID := doc.Chats[0].PlatformChatID
	entries, err := built.Run.Environment.Emulator.Journal(platformChatID)
	if err != nil {
		t.Fatalf("Journal() error = %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("Journal() returned no entries for a completed run")
	}

	refVerify := reference.Verify(entries)
	if built.VerifySpec == nil {
		t.Fatal("built.VerifySpec is nil, want the document's own compiled verify block")
	}
	docVerify := built.VerifySpec.Evaluate(entries)

	if docVerify.Verified != refVerify.Verified {
		t.Errorf("document verify.Verified = %v, want %v (arena's verifyGreetbotJournal)", docVerify.Verified, refVerify.Verified)
	}
	if docVerify.Detail != refVerify.Detail {
		t.Errorf("document verify.Detail =\n  %q\nwant (arena's verifyGreetbotJournal):\n  %q", docVerify.Detail, refVerify.Detail)
	}
	if !refVerify.Verified {
		t.Fatalf("reference.Verify() did not verify against this run's own journal — the fixture cassette no longer drives the scripted conversation to completion: refVerify=%+v", refVerify)
	}
}

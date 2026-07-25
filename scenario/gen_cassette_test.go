//go:build generate_greetbot_cassette

package scenario

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/examples/greetbot"
	"chatwright.dev/runtime/goal"
	"chatwright.dev/runtime/observe"
	"chatwright.dev/runtime/platform"
	"chatwright.dev/runtime/run"
	"chatwright.dev/runtime/telegram"
)

// TestGenerateGreetbotCassette is a one-shot generator, not a regression
// test (see the build tag): it drives the real greetbot fixture with a
// deterministic ScriptedProvider recorded through a CassetteProvider, and
// saves the result to testdata/cassettes/greetbot-language-onboarding.json
// — the fixture greetbot_conformance_test.go replays from. Run it with
// `go test -tags generate_greetbot_cassette -run TestGenerateGreetbotCassette ./scenario/...`
// only when the greetbot fixture's own conversation shape changes.
func TestGenerateGreetbotCassette(t *testing.T) {
	const chatID = int64(42)
	user := platform.User{ID: 7, FirstName: "Arena"}

	emu := telegram.NewEmulator()
	defer emu.Close()
	bot := greetbot.New(emu.BotAPIURL(), "TEST:TOKEN")
	srv := httptest.NewServer(bot.Handler())
	defer srv.Close()
	emu.SetWebhook(srv.URL, http.DefaultClient)

	if err := emu.SubmitText(chatID, user, "/start"); err != nil {
		t.Fatalf("dry run SubmitText() error = %v", err)
	}
	engine := observe.NewEngine(emu, observe.ChatRef{ChatID: chatID})
	obs, err := engine.Observe()
	if err != nil {
		t.Fatalf("dry run Observe() error = %v", err)
	}
	var englishActionID string
	for _, m := range obs.Messages {
		for _, a := range m.Actions {
			if a.Label == "English" {
				englishActionID = a.ID
			}
		}
	}
	if englishActionID == "" {
		t.Fatalf("dry run found no \"English\" action among %+v", obs.Messages)
	}
	emu.Close()
	srv.Close()

	// Fresh session for the actual recording.
	emu2 := telegram.NewEmulator()
	defer emu2.Close()
	bot2 := greetbot.New(emu2.BotAPIURL(), "TEST:TOKEN")
	srv2 := httptest.NewServer(bot2.Handler())
	defer srv2.Close()
	emu2.SetWebhook(srv2.URL, http.DefaultClient)

	live := actor.NewScriptedProvider(
		actor.Usage{Model: "scripted-conformance-v1", InputTokens: 12, OutputTokens: 4},
		actor.Proposal{Kind: actor.ProposeSendText, Text: "/start", Rationale: "open the language picker"},
		actor.Proposal{Kind: actor.ProposeClick, ActionID: englishActionID, ObservationSequence: 1, Rationale: "pick English"},
		actor.Proposal{Kind: actor.ProposeSendText, Text: "Thanks!", Rationale: "acknowledge the greeting"},
		actor.Proposal{Kind: actor.ProposeTaskDone, Rationale: "sent the acknowledgement"},
	)
	cassette := actor.NewCassette("greetbot-conformance-v1")
	cp, err := actor.NewCassetteProvider(actor.ModeRecord, live, cassette)
	if err != nil {
		t.Fatalf("NewCassetteProvider() error = %v", err)
	}

	g := goal.Goal{
		ID:    "language-onboarding-arena",
		Title: "Complete language onboarding and acknowledge the greeting",
		Tasks: []goal.Task{{
			ID:    "language-onboarding",
			Title: "Complete language onboarding",
			SuccessCriteria: `Send "/start" as text to begin the conversation. Wait for the bot's ` +
				`language picker message, which carries labelled available actions. Click the ` +
				`action labelled exactly "English" (a click proposal, using its listed action id) ` +
				`— do not send free text for this step, and do not click any other label. After ` +
				`the bot's greeting message changes (it is edited in place to an English greeting), ` +
				`send one short text message acknowledging the greeting (for example "Thanks!" or ` +
				`"Great, thanks for the greeting!"). Only once you have sent that acknowledgement ` +
				`should you declare the task done.`,
		}},
		Budgets: goal.Budgets{MaxSteps: 12, MaxDuration: 240 * time.Second},
	}

	part := run.NewAIGoalPart("language-onboarding", "Complete language onboarding", "", run.AIGoalPartInput{
		ActorID: "arena", Goal: g, Provider: cp,
		Config: actor.Config{ChatID: chatID, User: user},
	})
	// A frozen clock — never time.Now — so every LoopEvent.At this
	// recording produces is identical to what scenario.Build's own
	// defaultClock uses when replaying a cassette-only document: the
	// cache key for step 2 onward is a hash that includes Prompt.History,
	// which carries each prior LoopEvent.At. See scenario.defaultClock's
	// own doc comment for why any two separate cassette-replay runs must
	// agree on this exact instant.
	frozen := func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	r := run.Run{
		ID:          "gen-cassette",
		Environment: run.Environment{Emulator: emu2, ChatIDs: []int64{chatID}, Now: frozen},
		Parts:       []run.Part{part},
	}
	result, err := r.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(result.Parts) != 1 || result.Parts[0].Status != run.PartCompleted {
		t.Fatalf("result = %+v, want one completed part", result)
	}

	if err := cp.Cassette().Save("testdata/cassettes/greetbot-language-onboarding.json"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	t.Logf("wrote testdata/cassettes/greetbot-language-onboarding.json")
}

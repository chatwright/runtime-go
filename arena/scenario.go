package arena

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"chatwright.dev/runtime/examples/greetbot"
	"chatwright.dev/runtime/goal"
	"chatwright.dev/runtime/platform"
	"chatwright.dev/runtime/telegram"
	"chatwright.dev/sdk"
)

// Scenario declares one benchmarkable task: a Goal every provider/repeat
// attempts identically, the platform environment it runs against, and an
// optional deterministic re-check of completion, independent of the
// model's own task-done claim ("evidence over claims" — spec/ideas/
// actor-model-arena.md's "Quality stays objective" rule). ID and Version
// are recorded in every Results — the spec's groundwork for a future
// canonical-scenario registry ("no registry yet"): two Results sharing the
// same ID+Version ran the identical scenario and are provably
// apples-to-apples comparable; anything else is not.
type Scenario struct {
	// ID is this scenario's stable identity, e.g.
	// "greetbot-language-onboarding".
	ID string
	// Version is this scenario's own revision, independent of this
	// package's version — bump it whenever Goal, Setup or Verify changes
	// in a way that makes an old and a new run no longer comparable.
	Version string
	// Title is a human-readable one-line summary for the report header.
	Title string

	// Goal is the goal.Goal every provider/repeat attempts, identically —
	// including its own Budgets, used unless Matrix.Budgets overrides them
	// (see Matrix.Budgets).
	Goal goal.Goal

	// Setup boots a fresh platform environment (an emulator plus a
	// bot-under-test wired to it) for exactly one cell — called once per
	// timed repeat, and once more (untimed) for the mandatory warm-up.
	// Every call gets fresh state: no cell, and no warm-up call, ever
	// observes another call's conversation history.
	Setup func() (*ScenarioSession, error)

	// Verify optionally re-derives completion from the chat's raw journal
	// after a cell finishes, independent of whatever the actor itself
	// proposed (ProposeTaskDone) — see VerifyResult. Nil means a cell's
	// only completion signal is its own Report TaskOutcome.Status (the
	// model's self-declared claim, unverified).
	Verify func(entries []platform.JournalEntry) VerifyResult
}

// VerifyResult is one Scenario.Verify call's deterministic verdict.
type VerifyResult struct {
	// Verified is true only when the journal itself shows the scenario's
	// discriminating steps happened — never merely because the model
	// declared the task done.
	Verified bool
	// Detail is a short, human-readable explanation for the report's
	// per-model narrative: what the journal shows (or doesn't), in
	// evidence terms.
	Detail string
}

// ScenarioSession is one Scenario.Setup call's live environment: an
// isolated platform.Emulator with a fresh bot-under-test wired to it,
// ready to run exactly one campaign against.
type ScenarioSession struct {
	Emulator platform.Emulator
	ChatID   int64
	User     platform.User
	// BotActor is the roster entry for the bot-under-test this session
	// wired up — Run copies it into every cell's bundle roster verbatim,
	// alongside the ai-agent actor Run itself constructs per provider.
	BotActor sdk.Actor
	// Close tears down everything Setup started (the emulator's server,
	// the bot's own HTTP server). Always called by Run once the
	// cell/warm-up finishes, even on error.
	Close func()
}

// greetbotTaskID is the single task id GreetbotScenario's Goal declares.
const greetbotTaskID = "language-onboarding"

// englishGreeting is greetbot's exact reply text once "English" is picked
// (examples/greetbot/greetbot.go) — the deterministic signal
// verifyGreetbotJournal looks for; only clicking "English" specifically
// (not "Español"/"Français") produces this text.
const englishGreeting = "Howdy stranger"

// greetbotToken is the fake bot token GreetbotScenario's emulator/bot pair
// use — never a real credential, this is a Telegram emulator.
const greetbotToken = "TEST:TOKEN"

// GreetbotScenario is the arena's built-in first scenario (spec/ideas/
// actor-model-arena.md's MVP scope), ported from the scratchpad harness
// that produced the first actor-model arena report (chatwright/backstage
// research/model-arena-2026-07-23): send "/start", read the observed
// actions and click the one labelled exactly "English" among three
// choices, recognise the greeting has changed (an in-place edit, not a new
// message — that first run's own most consistent finding was a model
// re-clicking the button after it already worked because a naive
// non-progress detector cannot tell "the bot re-edited its own message" in
// place apart from "genuinely new progress"), and acknowledge it with free
// text before declaring done. One task, three distinct actor capabilities,
// identical across every provider in the matrix.
func GreetbotScenario() Scenario {
	return Scenario{
		ID:      "greetbot-language-onboarding",
		Version: "v1",
		Title:   "Complete language onboarding and acknowledge the greeting",
		Goal:    greetbotGoal(),
		Setup:   setupGreetbot,
		Verify:  verifyGreetbotJournal,
	}
}

func greetbotGoal() goal.Goal {
	return goal.Goal{
		ID:    "language-onboarding-arena",
		Title: "Complete language onboarding and acknowledge the greeting",
		Tasks: []goal.Task{
			{
				ID:    greetbotTaskID,
				Title: "Complete language onboarding",
				SuccessCriteria: `Send "/start" as text to begin the conversation. Wait for the bot's ` +
					`language picker message, which carries labelled available actions. Click the ` +
					`action labelled exactly "English" (a click proposal, using its listed action id) ` +
					`— do not send free text for this step, and do not click any other label. After ` +
					`the bot's greeting message changes (it is edited in place to an English greeting), ` +
					`send one short text message acknowledging the greeting (for example "Thanks!" or ` +
					`"Great, thanks for the greeting!"). Only once you have sent that acknowledgement ` +
					`should you declare the task done.`,
			},
		},
		Budgets: goal.Budgets{MaxSteps: 12, MaxDuration: 4 * time.Minute},
	}
}

// setupGreetbot boots a fresh Telegram emulator plus a real greetbot
// (examples/greetbot) wired to it over HTTP — the same pattern
// run/bundle_e2e_test.go and the scratchpad harness's cmd/run-cell/main.go
// both use.
func setupGreetbot() (*ScenarioSession, error) {
	const chatID = int64(42)
	user := platform.User{ID: 7, FirstName: "Arena"}

	emu := telegram.NewEmulator()
	bot := greetbot.New(emu.BotAPIURL(), greetbotToken)
	srv := httptest.NewServer(bot.Handler())
	emu.SetWebhook(srv.URL, http.DefaultClient)

	return &ScenarioSession{
		Emulator: emu,
		ChatID:   chatID,
		User:     user,
		BotActor: sdk.Actor{
			ID: "greetbot", Type: sdk.ActorBot, Name: "GreetBot",
			PlatformIdentities: map[string]sdk.PlatformIdentity{
				"telegram": {UserID: telegram.EmulatedBotUserID, FirstName: "GreetBot"},
			},
		},
		Close: func() {
			srv.Close()
			emu.Close()
		},
	}, nil
}

// verifyGreetbotJournal is the deterministic, journal-level check of what
// actually happened in the conversation — independent of whether the model
// itself declared the task done (evidence over claims). It looks for
// exactly the three discriminating steps the goal describes: the user sent
// "/start", a bot message was edited to the English greeting (which only
// happens if "English" specifically was clicked — Spanish/French produce
// different text), and a further user text followed it. Ported from the
// scratchpad harness's cmd/report/main.go verifyJournal.
func verifyGreetbotJournal(entries []platform.JournalEntry) VerifyResult {
	var sawStart, greetingChanged, ackAfterGreet bool
	greetIdx := -1
	for i, e := range entries {
		if e.Kind != platform.JournalEntryMessage {
			continue
		}
		switch e.Direction {
		case platform.DirectionUser:
			if e.Text == "/start" {
				sawStart = true
			}
			if greetIdx >= 0 && strings.TrimSpace(e.Text) != "" && e.Text != "/start" {
				ackAfterGreet = true
			}
		case platform.DirectionBot:
			if greetIdx < 0 && e.Version > 0 && e.Text == englishGreeting {
				greetingChanged = true
				greetIdx = i
			}
		}
	}

	verified := sawStart && greetingChanged && ackAfterGreet
	if verified {
		return VerifyResult{Verified: true, Detail: "started, clicked English, acknowledged — all journal-verified"}
	}

	var missing []string
	if !sawStart {
		missing = append(missing, "never sent /start")
	}
	if !greetingChanged {
		missing = append(missing, `never got the English greeting (wrong/no click)`)
	}
	if !ackAfterGreet {
		missing = append(missing, "never sent an acknowledgement after the greeting changed")
	}
	return VerifyResult{Verified: false, Detail: "journal evidence incomplete: " + strings.Join(missing, "; ")}
}

package campaign_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chatwright/chatwright/actor"
	"github.com/chatwright/chatwright/campaign"
	"github.com/chatwright/chatwright/examples/greetbot"
	"github.com/chatwright/chatwright/goal"
	"github.com/chatwright/chatwright/observe"
	"github.com/chatwright/chatwright/platform"
	"github.com/chatwright/chatwright/telegram"
)

// TestScriptedCampaignAgainstGreetbotEndToEnd is the slice-2 plan's gate: a
// ScriptedProvider campaign against the greetbot fixture, driving send and
// click through the REAL Telegram emulator (not a fake), all the way to a
// completed campaign.Report — proving actor.Loop and campaign.Assemble
// compose into a runnable, evidence-backed campaign, at zero token cost and
// zero flakiness (no network, no live model).
//
// greetbot is a genuine tgbotapi-protocol bot (examples/greetbot): /start
// offers a language choice whose selection EDITS the choice message in
// place — so this test also exercises the loop's in-place-edit handling
// (Loop.refreshRawBotMessage's WaitForEdit path), not just new messages.
//
// ScriptedProvider's script needs the real, opaque observe.AvailableAction
// ID for the "English" button before the actual run starts. That ID is
// deterministic (Telegram message IDs are a simple per-chat counter
// starting at 1, so an identical bot driven through an identical opening
// exchange on a second, independent emulator assigns identical IDs) but
// unknown ahead of time, so a short throwaway "dry run" against a second
// emulator instance learns it first — never by parsing observe's internal
// ID format.
func TestScriptedCampaignAgainstGreetbotEndToEnd(t *testing.T) {
	const chatID = int64(42)
	user := platform.User{ID: 7, FirstName: "Explorer"}

	englishActionID := dryRunLearnEnglishActionID(t, chatID, user)

	emu := telegram.NewEmulator()
	t.Cleanup(emu.Close)
	bot := greetbot.New(emu.BotAPIURL(), "TEST:TOKEN")
	srv := httptest.NewServer(bot.Handler())
	t.Cleanup(srv.Close)
	emu.SetWebhook(srv.URL, http.DefaultClient)

	engine := observe.NewEngine(emu, observe.ChatRef{ChatID: chatID})

	g := goal.Goal{
		ID: "greetbot-language", Title: "Select a language and confirm the bot responds",
		Tasks: []goal.Task{{
			ID: "select-language", Title: "Pick English and confirm the greeting",
			SuccessCriteria: `the language-choice message is edited to show "Howdy stranger"`,
		}},
		Budgets: goal.Budgets{MaxSteps: 10, MaxDuration: time.Minute},
	}
	camp, err := goal.NewCampaignState(g, time.Now)
	if err != nil {
		t.Fatalf("NewCampaignState() error = %v", err)
	}

	provider := actor.NewScriptedProvider(actor.Usage{Model: "scripted-v1", InputTokens: 12, OutputTokens: 4},
		actor.Proposal{Kind: actor.ProposeSendText, Text: "/start", Rationale: "open the language picker"},
		actor.Proposal{Kind: actor.ProposeClick, ActionID: englishActionID, ObservationSequence: 1, Rationale: "pick English"},
		actor.Proposal{Kind: actor.ProposeTaskDone, Rationale: "the bot confirmed the greeting"},
	)

	loop, err := actor.NewLoop(provider, engine, emu, camp, g, actor.Config{ChatID: chatID, User: user, Now: time.Now})
	if err != nil {
		t.Fatalf("NewLoop() error = %v", err)
	}

	results, err := loop.RunCampaign(context.Background())
	if err != nil {
		t.Fatalf("RunCampaign() error = %v", err)
	}
	if len(results) != 1 || results[0].Status != goal.TaskCompleted {
		t.Fatalf("RunCampaign() results = %+v, want one completed task", results)
	}

	// The click really went over the wire: the real bot edited its message.
	if transcript := emu.Transcript(chatID); !strings.Contains(transcript, "Howdy stranger") {
		t.Fatalf("emulator transcript does not show the bot's confirmed greeting:\n%s", transcript)
	}

	report := campaign.Assemble(campaign.AssembleInput{Goal: g, Campaign: camp.Snapshot(), Events: loop.Events()})

	if report.SchemaVersion != campaign.ReportSchemaVersion {
		t.Fatalf("report.SchemaVersion = %d, want %d", report.SchemaVersion, campaign.ReportSchemaVersion)
	}
	if report.StopReason != string(goal.StopGoalComplete) {
		t.Fatalf("report.StopReason = %q, want %q", report.StopReason, goal.StopGoalComplete)
	}
	if len(report.Tasks) != 1 || report.Tasks[0].Status != string(goal.TaskCompleted) || !report.Tasks[0].Attempted {
		t.Fatalf("report.Tasks = %+v, want one attempted, completed task", report.Tasks)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("report.Findings = %+v, want none for a clean success", report.Findings)
	}
	if report.Usage.CallCount != 3 {
		t.Fatalf("report.Usage.CallCount = %d, want 3 (one per Propose call)", report.Usage.CallCount)
	}
	if report.Usage.InputTokens != 36 || report.Usage.OutputTokens != 12 {
		t.Fatalf("report.Usage = %+v, want InputTokens 36, OutputTokens 12 (3 x scripted usage)", report.Usage)
	}

	// The report is exactly what the plan requires it to be: an exported,
	// versioned JSON contract a consumer never needs this package to read.
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("Marshal(report) error = %v", err)
	}
	if !strings.Contains(string(encoded), `"schemaVersion":1`) {
		t.Fatalf("marshalled report does not carry a readable schemaVersion: %s", encoded)
	}
}

// dryRunLearnEnglishActionID drives greetbot's /start through a throwaway,
// independent Telegram emulator + Engine to learn the real observe
// AvailableAction ID for the "English" language button, so the actual test
// run's ScriptedProvider can be constructed with it up front. See the
// TestScriptedCampaignAgainstGreetbotEndToEnd doc comment for why this is
// deterministic across the two independent emulators.
func dryRunLearnEnglishActionID(t *testing.T, chatID int64, user platform.User) string {
	t.Helper()
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
	// obs.Messages holds both sides of the exchange: the user's own
	// "/start" and the bot's "Choose your language" reply with the language
	// buttons.
	for _, m := range obs.Messages {
		for _, a := range m.Actions {
			if a.Label == "English" {
				return a.ID
			}
		}
	}
	t.Fatalf("dry run found no \"English\" action among %+v", obs.Messages)
	return ""
}

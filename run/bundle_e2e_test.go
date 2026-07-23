package run_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/campaign"
	"chatwright.dev/runtime/examples/greetbot"
	"chatwright.dev/runtime/goal"
	"chatwright.dev/runtime/observe"
	"chatwright.dev/runtime/platform"
	"chatwright.dev/runtime/run"
	"chatwright.dev/runtime/telegram"
	"chatwright.dev/sdk"
)

// TestScriptedCampaignBundleAgainstGreetbotEndToEnd is a sibling of
// campaign's frozen TestScriptedCampaignAgainstGreetbotEndToEnd that reuses
// the same flow (ScriptedProvider against the real greetbot fixture over the
// real Telegram emulator) and, once the campaign completes, assembles a real
// sdk.Bundle from the run's actual pieces — the emulator's own
// platform.Emulator.Journal, actor.Loop.Observations, actor.Loop.Events and
// the assembled campaign.Report, via run.SingleAIGoalRun (moved here from
// the pre-split bundle package; it wire-converts the runtime values
// internally) — writes it to a
// t.TempDir() file with sdk.Write, reads it back with sdk.Read, and
// checks the result structurally rather than against a byte-exact golden
// file: unlike TestBundleRoundTripIsDeterministic's hand-built Bundle, this
// run's platform.JournalEntry/actor.LoopEvent timestamps come from the real
// emulator/clock (telegram.Emulator has no injectable clock — see its
// journal-append sites), so no two runs produce identical bytes at that
// level. The write/read/write byte-identical check (which compares JSON
// produced from the same decoded value twice, so it never depends on the
// clock) still applies and is asserted here too.
//
// It additionally proves journal attribution end-to-end: every
// platform.JournalEntry with a non-zero FromID resolves, via a roster
// Actor's telegram PlatformIdentity, to exactly one actor — and the roster
// itself carries both a client-side actor (the scripted provider driving the
// conversation) and the bot-under-test.
func TestScriptedCampaignBundleAgainstGreetbotEndToEnd(t *testing.T) {
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

	// Retention is left at its default (on) — this is exactly the campaign
	// scenario Config.DisableObservationRetention's doc comment describes.
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

	entries, err := emu.Journal(chatID)
	if err != nil {
		t.Fatalf("Journal() error = %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("Journal() returned no entries for a completed campaign")
	}

	events := loop.Events()
	// The raw retained-observation map, exactly as run.SingleAIGoalRun's
	// input takes it — sorting (the old bundle package's SortObservations)
	// now happens inside SingleAIGoalRun.
	observations := loop.Observations()
	if len(observations) == 0 {
		t.Fatal("loop.Observations() is empty, want at least one retained observation (retention defaults on)")
	}

	report := campaign.Assemble(campaign.AssembleInput{Goal: g, Campaign: camp.Snapshot(), Events: events})

	// The roster: the scripted client-side actor that drove the
	// conversation (attributed via user.ID), and the bot-under-test itself
	// (attributed via telegram.EmulatedBotUserID, the id the emulator's
	// single simulated bot always answers getMe/sends/edits as).
	actors := []sdk.Actor{
		{
			ID: "explorer", Type: sdk.ActorScripted, Name: user.FirstName,
			PlatformIdentities: map[string]sdk.PlatformIdentity{
				"telegram": {UserID: user.ID, FirstName: user.FirstName},
			},
		},
		{
			ID: "greetbot", Type: sdk.ActorBot, Name: "ChatwrightBot",
			PlatformIdentities: map[string]sdk.PlatformIdentity{
				"telegram": {UserID: telegram.EmulatedBotUserID, FirstName: "ChatwrightBot"},
			},
		},
	}
	chats := []sdk.ChatJournal{run.WireJournal(chatID, entries)}

	bundleRun := run.SingleAIGoalRun(run.SingleAIGoalRunInput{
		RunID: "run-1", Platform: "telegram", EndpointProfile: sdk.EndpointProfilePlatformEmulated,
		Actors: actors, Chats: chats,
		PartID: "exploration", PartTitle: "Select a language and confirm the bot responds",
		ActorID:      "explorer",
		Goal:         g,
		Events:       events,
		Observations: observations,
		Report:       report,
	})

	b := sdk.Bundle{
		Format: sdk.FormatV1,
		Metadata: sdk.Metadata{
			// Caller-supplied, not time.Now — see Metadata.CreatedAt's own
			// doc comment; a fixed value keeps this one field deterministic
			// even though other retained timestamps (the real emulator's
			// journal, the real actor clock) are not.
			CreatedAt:         time.Date(2026, 7, 22, 15, 0, 0, 0, time.UTC),
			ChatwrightVersion: sdk.ModuleVersion(),
		},
		Runs: []sdk.Run{bundleRun},
	}

	// Every retained observation is keyed by exactly the Sequence it
	// carries, and every event's ObservationSequence resolves to one.
	bySequence := make(map[int64]bool, len(observations))
	for seq, obs := range observations {
		if obs.Sequence != seq {
			t.Fatalf("Observations() key = %d, but Observation.Sequence = %d", seq, obs.Sequence)
		}
		bySequence[seq] = true
	}
	for _, e := range events {
		if !bySequence[e.ObservationSequence] {
			t.Fatalf("LoopEvent %d references ObservationSequence %d, which is not among the retained observations", e.Index, e.ObservationSequence)
		}
	}

	// The chat's edit appears as a versioned entry sequence: the same
	// MessageID recorded at Version 0 (the original language-choice send)
	// and again at a later Version (the in-place edit to "Howdy stranger").
	versionsByMessage := make(map[int]map[int]string) // MessageID -> Version -> Text
	for _, e := range chats[0].Entries {
		if e.Direction != sdk.DirectionBot || e.Kind != sdk.JournalEntryMessage {
			continue
		}
		if versionsByMessage[e.MessageID] == nil {
			versionsByMessage[e.MessageID] = make(map[int]string)
		}
		versionsByMessage[e.MessageID][e.Version] = e.Text
	}
	editedMessageFound := false
	for messageID, versions := range versionsByMessage {
		if len(versions) < 2 {
			continue
		}
		if _, ok := versions[0]; !ok {
			continue
		}
		for version, text := range versions {
			if version > 0 && strings.Contains(text, "Howdy stranger") {
				editedMessageFound = true
				t.Logf("message %d: version 0 -> version %d edited its text to %q", messageID, version, text)
			}
		}
	}
	if !editedMessageFound {
		t.Fatalf("no bot message in the chat journal shows a versioned edit sequence ending in \"Howdy stranger\": %+v", versionsByMessage)
	}

	// Attribution end-to-end: every journal entry with a non-zero FromID
	// resolves to exactly one roster actor via its telegram
	// PlatformIdentity, and the roster carries both a client-side actor and
	// the bot.
	assertAttributionResolves(t, actors, entries)
	assertRosterHasClientAndBot(t, actors)

	// Write, read back, and prove the write/read/write cycle is
	// byte-identical (see the test's own doc comment for why this — not a
	// literal golden-file diff — is this test's determinism check).
	path := filepath.Join(t.TempDir(), "greetbot-language.chatwright.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := sdk.Write(f, b); err != nil {
		_ = f.Close()
		t.Fatalf("Write() error = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	readBack, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = readBack.Close() }()
	decoded, err := sdk.Read(readBack)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	if decoded.Format != sdk.FormatV1 {
		t.Fatalf("decoded.Format = %q, want %q", decoded.Format, sdk.FormatV1)
	}
	if len(decoded.Runs) != 1 {
		t.Fatalf("len(decoded.Runs) = %d, want 1", len(decoded.Runs))
	}
	decodedRun := decoded.Runs[0]
	if len(decodedRun.Chats) != 1 || decodedRun.Chats[0].ChatID != chatID || len(decodedRun.Chats[0].Entries) != len(entries) {
		t.Fatalf("decodedRun.Chats = %+v, want one chat (%d) with %d entries", decodedRun.Chats, chatID, len(entries))
	}
	if len(decodedRun.Parts) != 1 || decodedRun.Parts[0].Kind != sdk.PartKindAIGoal || decodedRun.Parts[0].AIGoal == nil {
		t.Fatalf("decodedRun.Parts = %+v, want one ai-goal part with a populated AIGoal section", decodedRun.Parts)
	}
	aiGoal := decodedRun.Parts[0].AIGoal
	if len(aiGoal.Observations) != len(observations) {
		t.Fatalf("decoded AIGoal.Observations has %d entries, want %d", len(aiGoal.Observations), len(observations))
	}
	if len(aiGoal.Events) != len(events) {
		t.Fatalf("decoded AIGoal.Events has %d entries, want %d", len(aiGoal.Events), len(events))
	}
	if aiGoal.Report.SchemaVersion != campaign.ReportSchemaVersion || aiGoal.Report.StopReason != string(goal.StopGoalComplete) {
		t.Fatalf("decoded AIGoal.Report = %+v, want a completed report matching campaign.ReportSchemaVersion", aiGoal.Report)
	}
	if decodedRun.Platform != "telegram" || decodedRun.EndpointProfile != sdk.EndpointProfilePlatformEmulated {
		t.Fatalf("decodedRun = %+v, want platform=telegram endpointProfile=%s", decodedRun, sdk.EndpointProfilePlatformEmulated)
	}
	if !decoded.Metadata.CreatedAt.Equal(b.Metadata.CreatedAt) {
		t.Fatalf("decoded.Metadata.CreatedAt = %v, want %v", decoded.Metadata.CreatedAt, b.Metadata.CreatedAt)
	}

	var rewritten bytes.Buffer
	if err := sdk.Write(&rewritten, decoded); err != nil {
		t.Fatalf("Write(decoded) error = %v", err)
	}
	if string(written) != rewritten.String() {
		t.Fatalf("write/read/write cycle is not byte-identical:\nfirst:\n%s\nsecond:\n%s", written, rewritten.String())
	}

	// A Bundle produced by this real campaign run — not the hand-built
	// golden fixture — also validates against the committed schema (read
	// from the resolved chatwright.dev/sdk module itself — see
	// sdkSchemaPath). This is the e2e counterpart the sdk repository's
	// TestGoldenBundleValidatesAgainstSchema's own doc comment refers to:
	// real timestamps, a real emulator-produced journal, and (unlike the
	// golden's always-populated slices) the loop's own nil/empty slices
	// wherever nothing occurred, exercising the schema's nullable-field
	// handling (the sdk's internal/schemagen) against genuine output rather
	// than only a curated fixture.
	validateBundleFile(t, compileSchema(t), path)
}

// assertAttributionResolves proves every entry with a non-zero FromID
// resolves, via exactly one roster actor's telegram PlatformIdentity, to
// that actor — the guarantee a player needs to attribute a transcript line
// to whoever sent it.
func assertAttributionResolves(t *testing.T, actors []sdk.Actor, entries []platform.JournalEntry) {
	t.Helper()

	byTelegramID := make(map[int64][]string) // telegram user id -> actor ids claiming it
	for _, a := range actors {
		identity, ok := a.PlatformIdentities["telegram"]
		if !ok {
			continue
		}
		byTelegramID[identity.UserID] = append(byTelegramID[identity.UserID], a.ID)
	}

	attributed := 0
	for i, e := range entries {
		if e.FromID == 0 {
			continue
		}
		claimants := byTelegramID[e.FromID]
		if len(claimants) != 1 {
			t.Fatalf("entry %d: FromID %d resolves to %d roster actors (%v), want exactly 1", i, e.FromID, len(claimants), claimants)
		}
		attributed++
	}
	if attributed == 0 {
		t.Fatal("no journal entry carried a non-zero FromID to attribute")
	}
}

// assertRosterHasClientAndBot proves the roster carries both a client-side
// actor (here, type ActorScripted for the ScriptedProvider that drove the
// conversation) and the bot-under-test (type ActorBot).
func assertRosterHasClientAndBot(t *testing.T, actors []sdk.Actor) {
	t.Helper()

	var hasScripted, hasBot bool
	for _, a := range actors {
		switch a.Type {
		case sdk.ActorScripted:
			hasScripted = true
		case sdk.ActorBot:
			hasBot = true
		}
	}
	if !hasScripted {
		t.Fatalf("roster %+v has no scripted client-side actor", actors)
	}
	if !hasBot {
		t.Fatalf("roster %+v has no bot actor", actors)
	}
}

// dryRunLearnEnglishActionID drives greetbot's /start through a throwaway,
// independent Telegram emulator + Engine to learn the real observe
// AvailableAction ID for the "English" language button, so the actual test
// run's ScriptedProvider can be constructed with it up front. Duplicated
// from campaign's frozen e2e_test.go (a different package, so it cannot
// share that unexported helper) — see
// TestScriptedCampaignAgainstGreetbotEndToEnd's doc comment there for why
// this is deterministic across the two independent emulators.
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

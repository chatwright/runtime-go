package campaign_test

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

	"github.com/chatwright/chatwright/actor"
	"github.com/chatwright/chatwright/campaign"
	"github.com/chatwright/chatwright/examples/greetbot"
	"github.com/chatwright/chatwright/goal"
	"github.com/chatwright/chatwright/observe"
	"github.com/chatwright/chatwright/platform"
	"github.com/chatwright/chatwright/telegram"
)

// TestScriptedCampaignBundleAgainstGreetbotEndToEnd is a sibling of
// TestScriptedCampaignAgainstGreetbotEndToEnd that reuses the same flow
// (ScriptedProvider against the real greetbot fixture over the real
// Telegram emulator) and, once the campaign completes, assembles a real
// campaign.Bundle from the run's actual pieces — the emulator's own
// platform.Emulator.Journal, actor.Loop.Observations, actor.Loop.Events and
// the assembled Report — writes it to a t.TempDir() file with WriteBundle,
// reads it back with ReadBundle, and checks the result structurally rather
// than against a byte-exact golden file: unlike TestBundleRoundTripIsDeterministic's
// hand-built Bundle, this run's platform.JournalEntry/actor.LoopEvent
// timestamps come from the real emulator/clock (telegram.Emulator has no
// injectable clock — see its journal-append sites), so no two runs produce
// identical bytes at that level. The write/read/write byte-identical check
// (which compares JSON produced from the same decoded value twice, so it
// never depends on the clock) still applies and is asserted here too.
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
	observations := campaign.SortObservations(loop.Observations())
	if len(observations) == 0 {
		t.Fatal("loop.Observations() is empty, want at least one retained observation (retention defaults on)")
	}

	report := campaign.Assemble(campaign.AssembleInput{Goal: g, Campaign: camp.Snapshot(), Events: events})

	bundle := campaign.Bundle{
		SchemaVersion: campaign.BundleSchemaVersion,
		Goal:          g,
		Chats:         []campaign.ChatJournal{{ChatID: chatID, Entries: entries}},
		Observations:  observations,
		Events:        events,
		Report:        report,
		Metadata: campaign.Metadata{
			// Caller-supplied, not time.Now — see Metadata.CreatedAt's own
			// doc comment; a fixed value keeps this one field deterministic
			// even though other retained timestamps (the real emulator's
			// journal, the real actor clock) are not.
			CreatedAt:         time.Date(2026, 7, 22, 15, 0, 0, 0, time.UTC),
			ChatwrightVersion: campaign.ModuleVersion(),
			Platform:          "telegram",
			EndpointProfile:   campaign.EndpointProfilePlatformEmulated,
			ModelIDs:          campaign.AggregateModelIDs(events),
		},
	}

	// Every retained observation is keyed by exactly the Sequence it
	// carries, and every event's ObservationSequence resolves to one.
	bySequence := make(map[int64]bool, len(observations))
	for _, ro := range observations {
		if ro.Observation.Sequence != ro.Sequence {
			t.Fatalf("RetainedObservation.Sequence = %d, but Observation.Sequence = %d", ro.Sequence, ro.Observation.Sequence)
		}
		bySequence[ro.Sequence] = true
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
	for _, e := range bundle.Chats[0].Entries {
		if e.Direction != platform.DirectionBot || e.Kind != platform.JournalEntryMessage {
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

	// Write, read back, and prove the write/read/write cycle is
	// byte-identical (see the test's own doc comment for why this — not a
	// literal golden-file diff — is this test's determinism check).
	path := filepath.Join(t.TempDir(), "bundle.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := campaign.WriteBundle(f, bundle); err != nil {
		_ = f.Close()
		t.Fatalf("WriteBundle() error = %v", err)
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
	defer readBack.Close()
	decoded, err := campaign.ReadBundle(readBack)
	if err != nil {
		t.Fatalf("ReadBundle() error = %v", err)
	}

	if decoded.SchemaVersion != campaign.BundleSchemaVersion {
		t.Fatalf("decoded.SchemaVersion = %d, want %d", decoded.SchemaVersion, campaign.BundleSchemaVersion)
	}
	if len(decoded.Chats) != 1 || decoded.Chats[0].ChatID != chatID || len(decoded.Chats[0].Entries) != len(entries) {
		t.Fatalf("decoded.Chats = %+v, want one chat (%d) with %d entries", decoded.Chats, chatID, len(entries))
	}
	if len(decoded.Observations) != len(observations) {
		t.Fatalf("decoded.Observations has %d entries, want %d", len(decoded.Observations), len(observations))
	}
	if len(decoded.Events) != len(events) {
		t.Fatalf("decoded.Events has %d entries, want %d", len(decoded.Events), len(events))
	}
	if decoded.Report.SchemaVersion != campaign.ReportSchemaVersion || decoded.Report.StopReason != string(goal.StopGoalComplete) {
		t.Fatalf("decoded.Report = %+v, want a completed report matching campaign.ReportSchemaVersion", decoded.Report)
	}
	if decoded.Metadata.Platform != "telegram" || decoded.Metadata.EndpointProfile != campaign.EndpointProfilePlatformEmulated {
		t.Fatalf("decoded.Metadata = %+v, want platform=telegram endpointProfile=%s", decoded.Metadata, campaign.EndpointProfilePlatformEmulated)
	}
	if !decoded.Metadata.CreatedAt.Equal(bundle.Metadata.CreatedAt) {
		t.Fatalf("decoded.Metadata.CreatedAt = %v, want %v", decoded.Metadata.CreatedAt, bundle.Metadata.CreatedAt)
	}
	if len(decoded.Metadata.ModelIDs) != 1 || decoded.Metadata.ModelIDs[0] != "scripted-v1" {
		t.Fatalf("decoded.Metadata.ModelIDs = %v, want [\"scripted-v1\"]", decoded.Metadata.ModelIDs)
	}

	var rewritten bytes.Buffer
	if err := campaign.WriteBundle(&rewritten, decoded); err != nil {
		t.Fatalf("WriteBundle(decoded) error = %v", err)
	}
	if string(written) != rewritten.String() {
		t.Fatalf("write/read/write cycle is not byte-identical:\nfirst:\n%s\nsecond:\n%s", written, rewritten.String())
	}
}

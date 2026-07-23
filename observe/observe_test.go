package observe_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"chatwright.dev/runtime/observe"
	"chatwright.dev/runtime/platform"
	"chatwright.dev/runtime/telegram"
)

// --- test helpers: drive the real Telegram emulator's fake Bot API exactly
// as a real bot's tgbotapi client would, so tests that need "a real
// send-then-edit through the Telegram emulator" go through the actual wire
// surface rather than poking journal internals. ---

// button is the wire shape of one Telegram inline keyboard button, used to
// build reply_markup payloads by hand (mirrors tgbotapi.InlineKeyboardButton's
// JSON tags).
type button struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
}

// postJSON POSTs a JSON payload to the fake Telegram Bot API and returns the
// decoded "result" object, failing the test if the call was rejected.
func postJSON(t *testing.T, url string, payload any) map[string]any {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var env struct {
		OK     bool           `json:"ok"`
		Result map[string]any `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode response from %s: %v", url, err)
	}
	if !env.OK {
		t.Fatalf("POST %s: bot API returned ok=false", url)
	}
	return env.Result
}

// sendMessage POSTs sendMessage as a real bot would, optionally with an
// inline keyboard, and returns the assigned message ID.
func sendMessage(t *testing.T, botAPIURL string, chatID int64, text string, keyboard [][]button) int {
	t.Helper()
	payload := map[string]any{"chat_id": chatID, "text": text}
	if keyboard != nil {
		payload["reply_markup"] = map[string]any{"inline_keyboard": keyboard}
	}
	result := postJSON(t, botAPIURL+"/botTEST:TOKEN/sendMessage", payload)
	id, _ := result["message_id"].(float64)
	return int(id)
}

// editMessage POSTs editMessageText as a real bot would.
func editMessage(t *testing.T, botAPIURL string, chatID int64, messageID int, text string, keyboard [][]button) {
	t.Helper()
	payload := map[string]any{"chat_id": chatID, "message_id": messageID, "text": text}
	if keyboard != nil {
		payload["reply_markup"] = map[string]any{"inline_keyboard": keyboard}
	}
	postJSON(t, botAPIURL+"/botTEST:TOKEN/editMessageText", payload)
}

// fakeJournaler is a Journaler over a caller-controlled, mutable entry set —
// used to exercise Engine transitions (like available actions changing
// without a version bump) that no current platform emulator produces
// directly, since the Observation Model's contract does not depend on any
// one platform's edit mechanics.
type fakeJournaler struct {
	entries []platform.JournalEntry
}

func (f *fakeJournaler) Journal(int64) ([]platform.JournalEntry, error) {
	return append([]platform.JournalEntry(nil), f.entries...), nil
}

// TestObservationMessageIdentityStableAcrossEdits drives a real
// send-then-edit through the Telegram emulator and checks the Engine
// observes one logical message with a bumped version and an explicit edited
// change — never two messages.
func TestObservationMessageIdentityStableAcrossEdits(t *testing.T) {
	e := telegram.NewEmulator()
	t.Cleanup(e.Close)

	const chatID = int64(7)
	msgID := sendMessage(t, e.BotAPIURL(), chatID, "Hello", nil)

	engine := observe.NewEngine(e, observe.ChatRef{ChatID: chatID})
	first, err := engine.Observe()
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(first.Messages) != 1 {
		t.Fatalf("first observation has %d messages, want 1", len(first.Messages))
	}
	if got := first.Messages[0]; got.Version != 0 || got.Edited {
		t.Fatalf("first observation message = %+v, want Version 0, Edited false", got)
	}
	firstID := first.Messages[0].ID

	editMessage(t, e.BotAPIURL(), chatID, msgID, "Hello, edited", nil)

	second, err := engine.Observe()
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(second.Messages) != 1 {
		t.Fatalf("second observation has %d messages, want 1 (one logical message, not two)", len(second.Messages))
	}
	got := second.Messages[0]
	if got.ID != firstID {
		t.Fatalf("edited message ID = %q, want it to stay %q (stable logical identity)", got.ID, firstID)
	}
	if !got.Edited || got.Version != 1 {
		t.Fatalf("edited message = %+v, want Edited true, Version 1", got)
	}
	if got.Text != "Hello, edited" {
		t.Fatalf("edited message text = %q, want %q", got.Text, "Hello, edited")
	}

	if len(second.Changes) != 1 {
		t.Fatalf("second observation has %d changes, want exactly 1: %+v", len(second.Changes), second.Changes)
	}
	ch := second.Changes[0]
	if ch.Kind != observe.ChangeMessageEdited || ch.MessageID != firstID || ch.PreviousVersion != 0 || ch.Version != 1 {
		t.Fatalf("change = %+v, want a single edited-message change for %q (v0 -> v1)", ch, firstID)
	}
}

// TestObservationChangesAreExplicit proves an actor is handed the semantic
// differences between two Observations — a new message and an
// actions-changed update to an existing one — as explicit, structured
// Changes, rather than being required to diff the two Observations' Messages
// itself.
func TestObservationChangesAreExplicit(t *testing.T) {
	fj := &fakeJournaler{entries: []platform.JournalEntry{
		{
			Direction: platform.DirectionBot, Kind: platform.JournalEntryMessage,
			MessageID: 1, Text: "Pick one",
			Actions: [][]platform.Action{{{Label: "Yes", ID: "cb_yes"}}},
		},
	}}
	engine := observe.NewEngine(fj, observe.ChatRef{ChatID: 1})

	obs1, err := engine.Observe()
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(obs1.Changes) != 0 {
		t.Fatalf("first observation changes = %+v, want none", obs1.Changes)
	}
	if len(obs1.Messages) != 1 || len(obs1.Messages[0].Actions) != 1 {
		t.Fatalf("obs1.Messages = %+v, want one message with one action", obs1.Messages)
	}

	// Between obs1 and obs2: message 1 gets a second action without its
	// version advancing (actions-changed), and an unrelated new message
	// (from the user) appears.
	fj.entries = append(fj.entries,
		platform.JournalEntry{
			Direction: platform.DirectionBot, Kind: platform.JournalEntryMessage,
			MessageID: 1, Text: "Pick one",
			Actions: [][]platform.Action{{{Label: "Yes", ID: "cb_yes"}, {Label: "No", ID: "cb_no"}}},
		},
		platform.JournalEntry{
			Direction: platform.DirectionUser, Kind: platform.JournalEntryMessage,
			MessageID: 2, Text: "hi",
		},
	)

	obs2, err := engine.Observe()
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(obs2.Messages) != 2 {
		t.Fatalf("obs2 has %d messages, want 2", len(obs2.Messages))
	}

	var newMsg, actionsMsg observe.VisibleMessage
	for _, m := range obs2.Messages {
		switch m.Text {
		case "hi":
			newMsg = m
		case "Pick one":
			actionsMsg = m
		}
	}
	if actionsMsg.ID != obs1.Messages[0].ID {
		t.Fatalf("message identity changed across an actions-only update: got %q, want %q", actionsMsg.ID, obs1.Messages[0].ID)
	}
	if actionsMsg.Version != obs1.Messages[0].Version || actionsMsg.Edited {
		t.Fatalf("actions-only update advanced the version: got %+v", actionsMsg)
	}
	if len(actionsMsg.Actions) != 2 {
		t.Fatalf("actionsMsg.Actions = %+v, want 2 actions after the update", actionsMsg.Actions)
	}

	var sawNew, sawActionsChanged bool
	for _, ch := range obs2.Changes {
		switch {
		case ch.Kind == observe.ChangeNewMessage && ch.MessageID == newMsg.ID:
			sawNew = true
		case ch.Kind == observe.ChangeActionsChanged && ch.MessageID == actionsMsg.ID:
			sawActionsChanged = true
		}
	}
	if !sawNew || !sawActionsChanged || len(obs2.Changes) != 2 {
		t.Fatalf("changes = %+v, want exactly a new-message change for %q and an actions-changed change for %q",
			obs2.Changes, newMsg.ID, actionsMsg.ID)
	}
}

// TestStaleActionProposalIsDetected drives a real Telegram emulator: an
// action is fresh immediately after being observed, then the bot replaces
// its available actions, and validating the original proposal against the
// engine's current journal deterministically reports it stale — Chatwright
// never blindly executes it. An unknown observation sequence is stale too.
func TestStaleActionProposalIsDetected(t *testing.T) {
	e := telegram.NewEmulator()
	t.Cleanup(e.Close)

	const chatID = int64(9)
	keyboard := [][]button{{{Text: "Yes", CallbackData: "cb_yes"}, {Text: "No", CallbackData: "cb_no"}}}
	msgID := sendMessage(t, e.BotAPIURL(), chatID, "Continue?", keyboard)

	engine := observe.NewEngine(e, observe.ChatRef{ChatID: chatID})
	obs1, err := engine.Observe()
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(obs1.Messages) != 1 || len(obs1.Messages[0].Actions) != 2 {
		t.Fatalf("obs1.Messages = %+v, want one message with 2 actions", obs1.Messages)
	}
	targetActionID := obs1.Messages[0].Actions[0].ID

	fresh, err := engine.Validate(observe.ActionProposal{ObservationSequence: obs1.Sequence, ActionID: targetActionID})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if fresh.Verdict != observe.VerdictFresh {
		t.Fatalf("verdict before any change = %v, want %v, reason: %s", fresh.Verdict, observe.VerdictFresh, fresh.Reason)
	}
	if fresh.Current == nil || fresh.Current.Label != "Yes" {
		t.Fatalf("fresh verdict Current = %+v, want Label %q", fresh.Current, "Yes")
	}

	// The bot replaces its available actions.
	editMessage(t, e.BotAPIURL(), chatID, msgID, "Never mind", nil)

	stale, err := engine.Validate(observe.ActionProposal{ObservationSequence: obs1.Sequence, ActionID: targetActionID})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if stale.Verdict != observe.VerdictStale {
		t.Fatalf("verdict after the message was edited = %v, want %v", stale.Verdict, observe.VerdictStale)
	}
	if stale.Reason == "" {
		t.Fatalf("stale verdict has no reason")
	}
	if stale.Current != nil {
		t.Fatalf("stale verdict Current = %+v, want nil", stale.Current)
	}

	unknown, err := engine.Validate(observe.ActionProposal{ObservationSequence: 999, ActionID: targetActionID})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if unknown.Verdict != observe.VerdictStale {
		t.Fatalf("verdict for an unknown observation sequence = %v, want %v", unknown.Verdict, observe.VerdictStale)
	}
}

// TestObservationHidesRawPlatformPayloads proves an Observation never carries
// Telegram's raw callback data — an actor sees only the action's label and a
// synthetic ID — while the same data remains reachable through the
// emulator's own Journal for developer trace/diagnostics.
func TestObservationHidesRawPlatformPayloads(t *testing.T) {
	e := telegram.NewEmulator()
	t.Cleanup(e.Close)

	const chatID = int64(11)
	const secretCallbackData = "super-secret-callback-payload-42"
	keyboard := [][]button{{{Text: "Book now", CallbackData: secretCallbackData}}}
	sendMessage(t, e.BotAPIURL(), chatID, "Ready to book?", keyboard)

	engine := observe.NewEngine(e, observe.ChatRef{ChatID: chatID})
	obs, err := engine.Observe()
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(obs.Messages) != 1 || len(obs.Messages[0].Actions) != 1 {
		t.Fatalf("obs.Messages = %+v, want one message with one action", obs.Messages)
	}
	action := obs.Messages[0].Actions[0]
	if action.Label != "Book now" {
		t.Fatalf("action label = %q, want %q", action.Label, "Book now")
	}
	if action.ID == secretCallbackData {
		t.Fatalf("action ID leaked the raw callback data: %q", action.ID)
	}

	// A structural sweep: nothing in the whole Observation's serialised form
	// contains the raw callback data, however deeply it might be nested.
	encoded, err := json.Marshal(obs)
	if err != nil {
		t.Fatalf("marshal observation: %v", err)
	}
	if strings.Contains(string(encoded), secretCallbackData) {
		t.Fatalf("observation JSON contains the raw callback data: %s", encoded)
	}

	// The raw callback data is withheld from the observation, not deleted:
	// it remains available through the emulator's own Journal/trace seam.
	journal, err := e.Journal(chatID)
	if err != nil {
		t.Fatalf("Journal: %v", err)
	}
	var sawRawCallbackData bool
	for _, en := range journal {
		for _, row := range en.Actions {
			for _, a := range row {
				if a.ID == secretCallbackData {
					sawRawCallbackData = true
				}
			}
		}
	}
	if !sawRawCallbackData {
		t.Fatalf("journal lost the raw callback data entirely; it should stay available to developer trace, just not to the observation")
	}
}

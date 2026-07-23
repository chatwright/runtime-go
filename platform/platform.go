// Package platform defines the neutral contracts that let a scenario be written
// once and executed against any chat platform.
//
// A scenario drives Chatwright with platform-agnostic verbs (send a text, expect
// a message, expect an action). Each Platform maps those neutral operations onto
// a concrete wire protocol (Telegram, WhatsApp, ...) and normalizes the bot's
// outbound calls back into the neutral Message/Action types below.
package platform

import (
	"net/http"
	"time"
)

// User is a neutral participant identity within a scenario.
type User struct {
	ID        int64
	FirstName string
	LastName  string
	Username  string
}

// Action is a neutral interactive action (a button) captured from a bot message.
// Telegram inline buttons and WhatsApp interactive replies both normalize to it.
type Action struct {
	Label string `json:"label"` // user-visible text (Telegram button text / WhatsApp reply title)
	ID    string `json:"id"`    // stable identifier (Telegram callback_data / WhatsApp reply id)
	URL   string `json:"url"`   // set for link actions
}

// Message is a neutral bot message captured from a platform's outbound API call.
type Message struct {
	Platform   string
	ChatID     int64
	MessageID  int // id the platform assigned to this outbound message
	Text       string
	Actions    [][]Action
	ReceivedAt time.Time
	Version    int // 0 for the original send; incremented on each in-place edit
}

// Direction identifies which side of a conversation produced a JournalEntry.
// It is a string type, not an int enum, so a JournalEntry marshals to
// human-readable JSON (see AGENTS.md's "JSON artefacts carry human-readable
// string constants" convention) rather than a bare, meaningless integer.
type Direction string

const (
	DirectionUser Direction = "user"
	DirectionBot  Direction = "bot"
)

// JournalEntryKind distinguishes what a JournalEntry records. It is a string
// type for the same reason as Direction — see Direction's doc comment.
type JournalEntryKind string

const (
	// JournalEntryMessage is an inbound user message or an outbound bot
	// send/edit; MessageID, Version, Text and Actions apply.
	JournalEntryMessage JournalEntryKind = "message"
	// JournalEntryAction is an inbound action activation (a button click or
	// equivalent interactive reply); RefMessageID names the message it
	// targeted, Text carries the platform action identifier that was
	// activated.
	JournalEntryAction JournalEntryKind = "action"
	// JournalEntryUncaptured records a bot API call the emulator does not
	// simulate — it produced no observable chat content; Method names the
	// call.
	JournalEntryUncaptured JournalEntryKind = "uncaptured"
)

// JournalEntry is one chronological, structured record from a chat's
// append-only journal — the same events an Emulator's Transcript renders as
// human-readable prose, given directly to callers (the observe package's
// Engine, diagnostics) that need to reason about them structurally instead of
// parsing rendered text. It carries the emulator's full internal record,
// including platform-native identifiers and action data (e.g. Telegram
// callback_data via Actions[*][*].ID) — this is the developer/trace-level
// seam, not the actor-facing observation surface; the observe package is
// where raw platform payloads are dropped before an actor ever sees them.
type JournalEntry struct {
	Direction    Direction        `json:"direction"`
	Kind         JournalEntryKind `json:"kind"`
	MessageID    int              `json:"messageId"`    // logical message identity, shared by inbound/outbound entries in this chat; 0 when Kind has no message identity of its own
	RefMessageID int              `json:"refMessageId"` // JournalEntryAction only: the message the action targeted
	Version      int              `json:"version"`      // JournalEntryMessage only: 0 = original send/inbound, N = the Nth edit
	Text         string           `json:"text"`
	Actions      [][]Action       `json:"actions"` // JournalEntryMessage only: actions attached to this entry, in platform row/col layout
	Method       string           `json:"method"`  // JournalEntryUncaptured only: the Bot API method name that was called
	At           time.Time        `json:"at"`

	// FromID is the platform-native identity of this entry's originator:
	// the Telegram user id of the client actor for a client-originated
	// entry, or the bot's own id for a bot-originated entry. It is 0 when
	// no identity is available (e.g. a pure method-call record with no
	// resolvable sender) — a Platform never invents an identity it does not
	// actually know. This is what lets a run-bundle roster (see the bundle
	// package's Actor.PlatformIdentities) attribute every journal entry to
	// whoever produced it.
	FromID int64 `json:"fromId"`
}

// Emulator is a running fake platform API server. It owns everything about
// getting a user action to the bot-under-test and capturing what comes back:
// building the platform-native update, assigning it identity (message and
// update IDs) from its own state, delivering it (webhook push, or queuing it
// for the bot to retrieve via getUpdates, on platforms that support polling),
// and recording every inbound and outbound event in an append-only per-chat
// journal. Current message state is derived from the journal; nothing is
// mutated in place, so edit history survives and a transcript can always be
// reconstructed. The harness (Chatwright) never builds wire bytes or POSTs
// them itself — it only submits neutral actions and waits for results.
type Emulator interface {
	// BotAPIURL is the base URL the bot-under-test must use for this platform's
	// API, in place of the real one (api.telegram.org, graph.facebook.com, ...).
	BotAPIURL() string

	// Close shuts the emulator's HTTP server down.
	Close()

	// SetWebhook registers the URL (and the HTTP client used to reach it) the
	// emulator pushes updates to, as the bot-under-test's webhook. Passing an
	// empty url clears it. On platforms that support polling (Telegram),
	// Submit* calls queue updates for getUpdates instead of failing while no
	// webhook is set; platforms that don't (WhatsApp) return an error from
	// Submit* until one is set — mirroring how a real bot token is either
	// webhook- or polling-driven, never both, and WhatsApp's Cloud API has no
	// polling mode at all.
	SetWebhook(url string, client *http.Client)

	// SubmitText delivers a user's text message from user in chatID to the
	// bot-under-test: it reserves the message's ID from chatID's shared
	// per-chat sequence, journals the inbound event, builds the
	// platform-native update, and delivers it via the configured strategy.
	// The returned error reports only a delivery failure (e.g. the webhook
	// responded with an error status, or no delivery strategy is available);
	// it is not a scenario assertion.
	SubmitText(chatID int64, user User, text string) error

	// SubmitClick delivers a user activating an interactive action (button
	// click) with the given callback data on the bot message identified by
	// targetMessageID. A callback query does not occupy its own slot in the
	// per-chat message-ID sequence.
	SubmitClick(chatID int64, user User, data string, targetMessageID int) error

	// WaitForMessage waits up to timeout for the (consumed+1)-th outbound bot
	// message to chatID, returning it normalized, or false on timeout.
	WaitForMessage(chatID int64, consumed int, timeout time.Duration) (*Message, bool)

	// WaitForEdit waits up to timeout for the message identified by
	// (chatID, messageID) to be edited past afterVersion (i.e. Version >
	// afterVersion), returning its new content normalized, or false on timeout.
	// Platforms with no message-edit capability may return false immediately.
	WaitForEdit(chatID int64, messageID int, afterVersion int, timeout time.Duration) (*Message, bool)

	// Transcript renders a chronological, human-readable dump of everything
	// recorded for chatID — inbound and outbound alike, edits shown as their
	// current version — for inclusion in assertion failure messages.
	Transcript(chatID int64) string

	// Journal returns chatID's chronological, structured journal entries —
	// the same events Transcript renders as prose, given directly to callers
	// (the observe package's Engine, diagnostics) that need to reason about
	// them structurally. An implementation that cannot produce a structured
	// journal returns a clear not-supported error and a nil slice.
	Journal(chatID int64) ([]JournalEntry, error)
}

// Platform maps neutral scenario operations onto a concrete chat platform.
type Platform interface {
	// Name is the platform's short identifier, e.g. "telegram" or "whatsapp".
	Name() string

	// Start boots this platform's emulated API server.
	Start() Emulator
}

// AddrPlatform is optionally implemented by a Platform whose emulated API
// server can bind a caller-chosen local address instead of a random port.
// It exists for a bot-under-test started as a separate process — in any
// language, since Chatwright only speaks HTTP — that must be configured
// with the emulator's API base URL in its environment before that process
// starts, which means the address has to be decided up front rather than
// read back from the emulator afterwards. chatwright.WithListenAddr uses
// this interface; New fails the test if a listen address is configured for
// a platform that doesn't implement it.
type AddrPlatform interface {
	Platform

	// StartAt boots this platform's emulated API server bound to addr (e.g.
	// "127.0.0.1:54321"). It returns an error if addr cannot be bound (e.g.
	// already in use) instead of panicking, so the caller can surface it
	// through the test.
	StartAt(addr string) (Emulator, error)
}

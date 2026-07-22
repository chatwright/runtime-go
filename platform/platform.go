// Package platform defines the neutral contracts that let a scenario be written
// once and executed against any chat platform.
//
// A scenario drives Chatwright with platform-agnostic verbs (send a text, expect
// a message, expect an action). Each Platform maps those neutral operations onto
// a concrete wire protocol (Telegram, WhatsApp, ...) and normalizes the bot's
// outbound calls back into the neutral Message/Action types below.
package platform

import "time"

// User is a neutral participant identity within a scenario.
type User struct {
	ID        int64
	FirstName string
	LastName  string
	Username  string
}

// Inbound is a neutral inbound text event that a Platform encodes as its own
// webhook payload before delivery to the bot-under-test.
type Inbound struct {
	ChatID    int64
	User      User
	Text      string
	UpdateID  int
	MessageID int
}

// InboundCallback is a neutral "button click": the user activating an interactive
// action on a bot message. A Platform encodes it as a Telegram callback query, a
// WhatsApp interactive reply, etc.
type InboundCallback struct {
	ChatID     int64
	User       User
	Data       string // the action's stable ID (Telegram callback_data / WhatsApp reply id)
	MessageID  int    // the bot message the action was attached to
	UpdateID   int
	CallbackID string
}

// Action is a neutral interactive action (a button) captured from a bot message.
// Telegram inline buttons and WhatsApp interactive replies both normalize to it.
type Action struct {
	Label string // user-visible text (Telegram button text / WhatsApp reply title)
	ID    string // stable identifier (Telegram callback_data / WhatsApp reply id)
	URL   string // set for link actions
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

// Emulator is a running fake platform API server. It records every inbound and
// outbound event in an append-only per-chat journal and captures the bot's
// outbound calls, normalized to the neutral types above. Current message state
// is derived from the journal; nothing is mutated in place, so edit history
// survives and a transcript can always be reconstructed.
type Emulator interface {
	// BotAPIURL is the base URL the bot-under-test must use for this platform's
	// API, in place of the real one (api.telegram.org, graph.facebook.com, ...).
	BotAPIURL() string

	// Close shuts the emulator's HTTP server down.
	Close()

	// RecordInboundText reserves the next message ID in chatID's shared
	// per-chat message-ID sequence — inbound and outbound messages draw from
	// the same counter, matching how Telegram scopes message_id per chat — and
	// appends an inbound journal entry for it. The caller embeds the returned
	// ID in the platform-native update it then builds with EncodeInboundText.
	RecordInboundText(chatID int64, user User, text string) (messageID int)

	// RecordInboundCallback appends a journal entry for the user activating an
	// interactive action (button) on the bot message identified by
	// targetMessageID. A callback query does not occupy its own slot in the
	// per-chat message-ID sequence.
	RecordInboundCallback(chatID int64, user User, data string, targetMessageID int)

	// EncodeInboundText encodes a user text message as this platform's webhook
	// payload, returning the content type and body to POST to the bot's webhook.
	EncodeInboundText(in Inbound) (contentType string, body []byte)

	// EncodeCallback encodes a button click (interactive action activation) as
	// this platform's webhook payload.
	EncodeCallback(in InboundCallback) (contentType string, body []byte)

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
}

// Platform maps neutral scenario operations onto a concrete chat platform.
type Platform interface {
	// Name is the platform's short identifier, e.g. "telegram" or "whatsapp".
	Name() string

	// Start boots this platform's emulated API server.
	Start() Emulator
}

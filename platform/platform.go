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
}

// Platform maps neutral scenario operations onto a concrete chat platform.
type Platform interface {
	// Name is the platform's short identifier, e.g. "telegram" or "whatsapp".
	Name() string

	// Start boots this platform's emulated API server.
	Start() Emulator
}

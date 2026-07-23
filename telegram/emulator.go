// Package telegram implements the Telegram Platform for Chatwright: an emulated
// Telegram Bot API server that delivers updates and captures the bot's outbound
// calls, normalized to Chatwright's neutral platform types.
//
// The Telegram wire types come from the bots-go-framework platform adapter
// github.com/bots-go-framework/bots-api-telegram (tgbotapi), so Chatwright parses
// and builds messages exactly as the framework does. The bot under test remains
// free to be written in any language or framework — this server only speaks the
// Telegram Bot API over HTTP.
package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bots-go-framework/bots-api-telegram/tgbotapi"

	"chatwright.dev/runtime/platform"
)

// Platform returns the Telegram platform for use with cw.OnPlatform.
func Platform() platform.Platform { return tgPlatform{} }

type tgPlatform struct{}

func (tgPlatform) Name() string { return "telegram" }

func (tgPlatform) Start() platform.Emulator { return NewEmulator() }

// StartAt implements platform.AddrPlatform: it boots the emulator bound to a
// caller-chosen address instead of a random port.
func (tgPlatform) StartAt(addr string) (platform.Emulator, error) { return NewEmulatorAt(addr) }

// journalDirection distinguishes who produced a journal entry.
type journalDirection int

const (
	fromUser journalDirection = iota
	fromBot
)

// journalKind distinguishes the shape of a journal entry.
type journalKind int

const (
	kindText        journalKind = iota // an inbound message or an outbound sendMessage/editMessageText
	kindCallback                       // an inbound button click
	kindUnsupported                    // a bot API call to a method the emulator does not simulate
)

// EmulatedBotUserID is the Telegram user id this emulator always assigns to
// the single bot-under-test it simulates — the same id getMe returns and
// every outbound (bot-originated) message/edit is sent "from". The emulator
// does not yet distinguish multiple bot identities within one instance (see
// journalEntry's own doc comment on its recorded-but-unused token field), so
// this is the one value a caller needs to attribute any bot-originated
// platform.JournalEntry (via its FromID) to the bot.
const EmulatedBotUserID int64 = 1

// journalEntry is one immutable entry in a chat's append-only event journal.
// A message edit never mutates a prior entry: it appends a new entry carrying
// the same messageID and an incremented version, so intermediate content and
// the true call order both survive. The "current" state of a messageID is its
// highest-version entry, derived by callers rather than stored separately.
type journalEntry struct {
	chatID       int64
	dir          journalDirection
	kind         journalKind
	messageID    int // own identity, shared by inbound and outbound messages in this chat; 0 for entries with no message identity of their own (callbacks, unsupported calls)
	refMessageID int // kindCallback only: the bot message the click targeted
	version      int // 0 = original send/inbound; N = the Nth edit of this messageID
	text         string
	markup       *tgbotapi.InlineKeyboardMarkup
	at           time.Time
	fromID       int64 // Telegram user id of this entry's originator: the sending user's id (fromUser) or EmulatedBotUserID (fromBot)

	// method and token are populated for bot-originated wire calls (sendMessage,
	// editMessageText, unsupported methods): method is the Bot API method name;
	// token is the bot token from the request path (/bot<token>/<method>),
	// unvalidated and not yet used to distinguish multiple bots — recorded now
	// so that seam exists when multi-bot identity is needed.
	method string
	token  string
}

// Emulator is an in-process HTTP server emulating the Telegram Bot API. It
// owns delivery of updates to the bot-under-test: SubmitText/SubmitClick
// build the platform-native update and either push it to a configured
// webhook or queue it for the bot to retrieve via getUpdates — the harness
// never builds wire bytes or POSTs them itself.
type Emulator struct {
	server *httptest.Server

	mu        sync.Mutex
	journal   []journalEntry
	nextMsgID map[int64]int // per-chat shared message-ID sequence, inbound and outbound alike
	updated   chan struct{} // closed (and replaced) on every journal append or queued update; broadcast signal

	nextUpdateID int
	webhookURL   string
	httpClient   *http.Client
	pending      []tgbotapi.Update // queued for getUpdates while no webhook is configured
}

// NewEmulator starts a fake Telegram Bot API server on a random local port.
func NewEmulator() *Emulator {
	e := &Emulator{
		nextMsgID: make(map[int64]int),
		updated:   make(chan struct{}),
	}
	e.server = httptest.NewServer(http.HandlerFunc(e.handle))
	return e
}

// NewEmulatorAt starts a fake Telegram Bot API server bound to addr (e.g.
// "127.0.0.1:54321") instead of a random local port. This is what lets an
// externally-started bot process — written in any language, since Chatwright
// only speaks HTTP — be configured with the emulator's exact API base URL
// before it starts: pick a free address once (e.g. bind to "127.0.0.1:0",
// read the assigned port back, then close it), pass that same address here
// and to the process's configuration, and the emulator ends up listening
// exactly where the process already expects it.
func NewEmulatorAt(addr string) (*Emulator, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("chatwright: listen on %q: %w", addr, err)
	}
	e := &Emulator{
		nextMsgID: make(map[int64]int),
		updated:   make(chan struct{}),
	}
	e.server = &httptest.Server{
		Listener: ln,
		Config:   &http.Server{Handler: http.HandlerFunc(e.handle)},
	}
	e.server.Start()
	return e, nil
}

// BotAPIURL is the base URL the bot-under-test should use as its Telegram Bot API
// host, in place of https://api.telegram.org.
func (e *Emulator) BotAPIURL() string { return e.server.URL }

// Close shuts down the emulator's HTTP server.
func (e *Emulator) Close() { e.server.Close() }

// SetWebhook registers the URL (and HTTP client) the emulator pushes updates
// to. Passing an empty url clears it, switching delivery back to queuing
// updates for getUpdates.
func (e *Emulator) SetWebhook(url string, client *http.Client) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.webhookURL = url
	e.httpClient = client
}

// reserveMessageIDLocked returns the next ID in chatID's shared message-ID
// sequence, starting at 1. Caller must hold e.mu.
func (e *Emulator) reserveMessageIDLocked(chatID int64) int {
	e.nextMsgID[chatID]++
	return e.nextMsgID[chatID]
}

// reserveUpdateIDLocked returns the next Bot API update_id, starting at 1.
// Caller must hold e.mu.
func (e *Emulator) reserveUpdateIDLocked() int {
	e.nextUpdateID++
	return e.nextUpdateID
}

// appendLocked appends entry to the journal and wakes any waiters. Caller must
// hold e.mu.
func (e *Emulator) appendLocked(entry journalEntry) {
	e.journal = append(e.journal, entry)
	e.wakeLocked()
}

// wakeLocked broadcasts a state-change signal to anything blocked waiting on
// e.updated (WaitForMessage, WaitForEdit, a long-polling getUpdates call).
// Caller must hold e.mu.
func (e *Emulator) wakeLocked() {
	close(e.updated)
	e.updated = make(chan struct{})
}

// latestTextEntryLocked returns the highest-version bot-sent text entry for
// (chatID, messageID) — i.e. its current, possibly-edited state. Caller must
// hold e.mu.
func (e *Emulator) latestTextEntryLocked(chatID int64, messageID int) (journalEntry, bool) {
	for i := len(e.journal) - 1; i >= 0; i-- {
		en := e.journal[i]
		if en.chatID == chatID && en.messageID == messageID && en.dir == fromBot && en.kind == kindText {
			return en, true
		}
	}
	return journalEntry{}, false
}

// SubmitText delivers a user's text message to the bot-under-test: it
// reserves the message's ID from chatID's shared sequence, journals the
// inbound event, builds the Telegram update, and delivers it.
func (e *Emulator) SubmitText(chatID int64, user platform.User, text string) error {
	e.mu.Lock()
	msgID := e.reserveMessageIDLocked(chatID)
	e.appendLocked(journalEntry{chatID: chatID, dir: fromUser, kind: kindText, messageID: msgID, text: text, at: time.Now(), fromID: user.ID})
	updateID := e.reserveUpdateIDLocked()
	webhookURL, client := e.webhookURL, e.httpClient
	e.mu.Unlock()

	update := tgbotapi.Update{
		UpdateID: updateID,
		Message: &tgbotapi.Message{
			MessageID: msgID,
			From: &tgbotapi.User{
				ID:        user.ID,
				FirstName: user.FirstName,
				LastName:  user.LastName,
				UserName:  user.Username,
			},
			Chat: &tgbotapi.Chat{ID: chatID, Type: "private", FirstName: user.FirstName},
			Date: int(time.Now().Unix()),
			Text: text,
		},
	}
	return e.deliver(update, webhookURL, client)
}

// SubmitClick delivers a user's button click (an interactive action
// activation) to the bot-under-test as a Telegram callback query. It does not
// reserve a message ID: a callback query references an existing message
// (targetMessageID) rather than creating a new one.
func (e *Emulator) SubmitClick(chatID int64, user platform.User, data string, targetMessageID int) error {
	e.mu.Lock()
	e.appendLocked(journalEntry{chatID: chatID, dir: fromUser, kind: kindCallback, refMessageID: targetMessageID, text: data, at: time.Now(), fromID: user.ID})
	updateID := e.reserveUpdateIDLocked()
	webhookURL, client := e.webhookURL, e.httpClient
	e.mu.Unlock()

	update := tgbotapi.Update{
		UpdateID: updateID,
		CallbackQuery: &tgbotapi.CallbackQuery{
			ID: "cb" + strconv.Itoa(updateID),
			From: &tgbotapi.User{
				ID:        user.ID,
				FirstName: user.FirstName,
				LastName:  user.LastName,
				UserName:  user.Username,
			},
			Data: data,
			Message: &tgbotapi.Message{
				MessageID: targetMessageID,
				From:      &tgbotapi.User{ID: EmulatedBotUserID, IsBot: true, FirstName: "ChatwrightBot"},
				Chat:      &tgbotapi.Chat{ID: chatID, Type: "private", FirstName: user.FirstName},
			},
		},
	}
	return e.deliver(update, webhookURL, client)
}

// deliver pushes update to webhookURL over HTTP if one is configured — this
// blocks until the bot-under-test's handler returns, exactly like a real
// webhook push, which is why every SendText/Click call in a scenario can
// assume the bot has already processed it by the time the call returns.
// Otherwise it queues the update for the bot to retrieve via getUpdates,
// mirroring how a real Telegram bot token is either webhook- or
// polling-driven, never both.
func (e *Emulator) deliver(update tgbotapi.Update, webhookURL string, client *http.Client) error {
	if webhookURL == "" {
		e.mu.Lock()
		e.pending = append(e.pending, update)
		e.wakeLocked()
		e.mu.Unlock()
		return nil
	}

	body, err := json.Marshal(update)
	if err != nil {
		return fmt.Errorf("chatwright: encode update: %w", err)
	}
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Post(webhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("chatwright: deliver update to webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("chatwright: webhook returned status %d", resp.StatusCode)
	}
	return nil
}

// acknowledgedMethods are Bot API calls the emulator does not simulate but
// must not error on: they produce no observable chat content (no message a
// scenario could assert on), and well-behaved bots routinely call them —
// registering or clearing a webhook, acknowledging a callback query (which
// stops the Telegram client's loading spinner), declaring commands. Anything
// else unrecognized is very likely a message-producing call (sendPhoto,
// sendDocument, ...) that chatwright silently dropping would mask a bug the
// scenario meant to catch, so it errors instead — see handleUnsupported.
var acknowledgedMethods = map[string]bool{
	"setWebhook":          true,
	"deleteWebhook":       true,
	"answerCallbackQuery": true,
	"setMyCommands":       true,
}

// handle routes /bot<token>/<method> like the real Bot API.
func (e *Emulator) handle(w http.ResponseWriter, r *http.Request) {
	token, method := parsePath(r.URL.Path)

	switch method {
	case "getMe":
		writeResult(w, tgbotapi.User{ID: EmulatedBotUserID, IsBot: true, FirstName: "ChatwrightBot", UserName: "chatwright_bot"})
	case "getUpdates":
		e.handleGetUpdates(w, r)
	case "sendMessage":
		e.handleSendMessage(w, r, token)
	case "editMessageText":
		e.handleEditMessageText(w, r, token)
	default:
		if acknowledgedMethods[method] {
			writeResult(w, true)
			return
		}
		e.handleUnsupported(w, r, token, method)
	}
}

// handleUnsupported responds to a Bot API method the emulator does not
// simulate with a Telegram-shaped error, and journals the call (attributed to
// whatever chat_id the request happened to carry, best-effort, if any) so a
// transcript can show, e.g., "bot also called sendPhoto (uncaptured)" instead
// of a bare "no message arrived" — the silent ok:true this replaces was
// exactly the kind of leniency that masks the bug chatwright exists to catch.
func (e *Emulator) handleUnsupported(w http.ResponseWriter, r *http.Request, token, method string) {
	chatID := parseGenericChatID(r)

	e.mu.Lock()
	e.appendLocked(journalEntry{chatID: chatID, dir: fromBot, kind: kindUnsupported, method: method, token: token, at: time.Now(), fromID: EmulatedBotUserID})
	e.mu.Unlock()

	writeUnsupported(w, method)
}

// handleGetUpdates emulates the Bot API's long-polling endpoint: a
// good-enough subset of https://core.telegram.org/bots/api#getupdates —
// offset acknowledges (updates with update_id < offset are considered
// received and dropped), and timeout (seconds) long-polls when nothing is
// pending yet.
func (e *Emulator) handleGetUpdates(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	offset, _ := strconv.Atoi(r.FormValue("offset"))
	timeoutSec, _ := strconv.Atoi(r.FormValue("timeout"))
	limit, _ := strconv.Atoi(r.FormValue("limit"))
	if limit <= 0 || limit > 100 {
		limit = 100
	}

	deadline := time.After(time.Duration(timeoutSec) * time.Second)
	for {
		e.mu.Lock()
		if offset > 0 {
			e.acknowledgeLocked(offset)
		}
		result := make([]tgbotapi.Update, 0, min(len(e.pending), limit))
		for _, u := range e.pending {
			if len(result) >= limit {
				break
			}
			result = append(result, u)
		}
		ch := e.updated
		e.mu.Unlock()

		if len(result) > 0 || timeoutSec <= 0 {
			writeResult(w, result)
			return
		}
		select {
		case <-ch:
		case <-deadline:
			writeResult(w, []tgbotapi.Update{})
			return
		}
	}
}

// acknowledgeLocked drops queued updates with UpdateID < offset: once a bot
// requests updates from offset, real Telegram considers everything before it
// received and will not resend it. Caller must hold e.mu.
func (e *Emulator) acknowledgeLocked(offset int) {
	kept := e.pending[:0]
	for _, u := range e.pending {
		if u.UpdateID >= offset {
			kept = append(kept, u)
		}
	}
	e.pending = kept
}

// handleSendMessage emulates sendMessage: it validates chat_id/text are
// present (a malformed or incomplete call is a real Bot API 400, not a
// silently-accepted no-op), assigns the message its ID from chatID's shared
// sequence, and returns a proper Message result carrying that ID.
func (e *Emulator) handleSendMessage(w http.ResponseWriter, r *http.Request, token string) {
	chatID, text, markup, err := parseSendMessage(r)
	if err != nil {
		writeError(w, err.Error())
		return
	}
	if chatID == 0 {
		writeError(w, "sendMessage: chat_id is required")
		return
	}
	if text == "" {
		writeError(w, "sendMessage: text is required")
		return
	}

	e.mu.Lock()
	messageID := e.reserveMessageIDLocked(chatID)
	at := time.Now()
	e.appendLocked(journalEntry{chatID: chatID, dir: fromBot, kind: kindText, messageID: messageID, text: text, markup: markup, at: at, method: "sendMessage", token: token, fromID: EmulatedBotUserID})
	e.mu.Unlock()

	writeResult(w, tgbotapi.Message{
		MessageID: messageID,
		From:      &tgbotapi.User{ID: EmulatedBotUserID, IsBot: true, FirstName: "ChatwrightBot"},
		Chat:      &tgbotapi.Chat{ID: chatID, Type: "private"},
		Date:      int(at.Unix()),
		Text:      text,
	})
}

// handleEditMessageText emulates editMessageText: it validates chat_id and
// message_id are present, then appends a new, versioned journal entry for
// the identified message (rather than mutating its prior entry), so
// WaitForEdit can observe the change and the transcript can show the
// message's full edit history was recorded, even though only its current
// state is displayed.
func (e *Emulator) handleEditMessageText(w http.ResponseWriter, r *http.Request, token string) {
	chatID, messageID, text, markup, err := parseEditMessageText(r)
	if err != nil {
		writeError(w, err.Error())
		return
	}
	if chatID == 0 || messageID == 0 {
		writeError(w, "editMessageText: chat_id and message_id are required")
		return
	}

	e.mu.Lock()
	prev, found := e.latestTextEntryLocked(chatID, messageID)
	if !found {
		e.mu.Unlock()
		writeError(w, "message to edit not found")
		return
	}
	if markup == nil {
		markup = prev.markup // editMessageText without reply_markup keeps the existing keyboard, like real Telegram
	}
	at := time.Now()
	e.appendLocked(journalEntry{
		chatID: chatID, dir: fromBot, kind: kindText,
		messageID: messageID, version: prev.version + 1,
		text: text, markup: markup, at: at,
		method: "editMessageText", token: token,
		fromID: EmulatedBotUserID,
	})
	e.mu.Unlock()

	writeResult(w, tgbotapi.Message{
		MessageID: messageID,
		From:      &tgbotapi.User{ID: EmulatedBotUserID, IsBot: true, FirstName: "ChatwrightBot"},
		Chat:      &tgbotapi.Chat{ID: chatID, Type: "private"},
		Date:      int(at.Unix()),
		Text:      text,
	})
}

// WaitForMessage waits for the (consumed+1)-th outbound message to chatID and
// returns its current (possibly-edited) state as a neutral platform.Message.
func (e *Emulator) WaitForMessage(chatID int64, consumed int, timeout time.Duration) (*platform.Message, bool) {
	deadline := time.After(timeout)
	for {
		e.mu.Lock()
		result := e.nthOutboundMessageLocked(chatID, consumed)
		ch := e.updated
		e.mu.Unlock()

		if result != nil {
			return result, true
		}
		select {
		case <-ch:
		case <-deadline:
			return nil, false
		}
	}
}

// nthOutboundMessageLocked returns the current (latest-version) state of the
// (consumed+1)-th distinct bot-sent message to chatID, in the order those
// messages were first sent. Caller must hold e.mu.
func (e *Emulator) nthOutboundMessageLocked(chatID int64, consumed int) *platform.Message {
	var order []int // messageIDs in first-seen order
	latest := map[int]journalEntry{}
	for _, en := range e.journal {
		if en.chatID != chatID || en.dir != fromBot || en.kind != kindText {
			continue
		}
		if _, ok := latest[en.messageID]; !ok {
			order = append(order, en.messageID)
		}
		latest[en.messageID] = en
	}
	if consumed >= len(order) {
		return nil
	}
	en := latest[order[consumed]]
	return normalize(&en)
}

// WaitForEdit waits for the message identified by (chatID, messageID) to be
// edited past afterVersion.
func (e *Emulator) WaitForEdit(chatID int64, messageID int, afterVersion int, timeout time.Duration) (*platform.Message, bool) {
	deadline := time.After(timeout)
	for {
		e.mu.Lock()
		var result *platform.Message
		if en, found := e.latestTextEntryLocked(chatID, messageID); found && en.version > afterVersion {
			result = normalize(&en)
		}
		ch := e.updated
		e.mu.Unlock()

		if result != nil {
			return result, true
		}
		select {
		case <-ch:
		case <-deadline:
			return nil, false
		}
	}
}

// Transcript renders a chronological, human-readable dump of everything
// recorded for chatID — inbound user messages, outbound bot messages (shown
// at their current, possibly-edited text) and button clicks — for inclusion
// in assertion failure messages. It is the emulator's own record, independent
// of what any BotMessage handle has consumed or asserted on. It renders from
// the same structured entries Journal returns, so the two never drift.
func (e *Emulator) Transcript(chatID int64) string {
	e.mu.Lock()
	entries := e.journalLocked(chatID)
	e.mu.Unlock()
	return renderTranscript(chatID, entries)
}

// Journal returns chatID's chronological, structured journal entries — the
// same events Transcript renders as prose, given directly to callers (the
// observe package's Engine, diagnostics) that need to reason about them
// structurally.
func (e *Emulator) Journal(chatID int64) ([]platform.JournalEntry, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.journalLocked(chatID), nil
}

// journalLocked returns chatID's structured journal entries in chronological
// order. Caller must hold e.mu.
func (e *Emulator) journalLocked(chatID int64) []platform.JournalEntry {
	entries := make([]platform.JournalEntry, 0, len(e.journal))
	for _, en := range e.journal {
		if en.chatID != chatID {
			continue
		}
		entries = append(entries, toPlatformEntry(en))
	}
	return entries
}

// toPlatformEntry converts an internal journalEntry into the platform-neutral
// platform.JournalEntry Journal exposes — the same event, described without
// this package's private wire representation (tgbotapi markup becomes
// neutral Actions; method/token wire detail stays only on the Uncaptured
// case, where it is the only content there is).
func toPlatformEntry(en journalEntry) platform.JournalEntry {
	pe := platform.JournalEntry{
		MessageID:    en.messageID,
		RefMessageID: en.refMessageID,
		Version:      en.version,
		Text:         en.text,
		At:           en.at,
		FromID:       en.fromID,
	}
	if en.dir == fromBot {
		pe.Direction = platform.DirectionBot
	} else {
		pe.Direction = platform.DirectionUser
	}
	switch en.kind {
	case kindCallback:
		pe.Kind = platform.JournalEntryAction
	case kindUnsupported:
		pe.Kind = platform.JournalEntryUncaptured
		pe.Method = en.method
	default:
		pe.Kind = platform.JournalEntryMessage
		pe.Actions = actionsFromMarkup(en.markup)
	}
	return pe
}

// renderTranscript renders entries (chatID's structured journal, in
// chronological order) as the prose Transcript returns: an edit updates its
// message's existing line in place instead of appending a new one, so a
// message keeps its original conversational position.
func renderTranscript(chatID int64, entries []platform.JournalEntry) string {
	var lines []string
	posByID := map[int]int{}
	for _, en := range entries {
		if en.Kind == platform.JournalEntryMessage {
			if i, ok := posByID[en.MessageID]; ok {
				lines[i] = renderJournalEntry(en) // an edit: update this message's line in place, no new line
				continue
			}
			posByID[en.MessageID] = len(lines)
		}
		lines = append(lines, renderJournalEntry(en))
	}

	if len(lines) == 0 {
		return fmt.Sprintf("chat %d transcript: (empty — no messages yet)", chatID)
	}
	return fmt.Sprintf("chat %d transcript: %s", chatID, strings.Join(lines, " / "))
}

// renderJournalEntry renders one transcript line for en.
func renderJournalEntry(en platform.JournalEntry) string {
	switch en.Kind {
	case platform.JournalEntryAction:
		return fmt.Sprintf("[user] clicked %q on message %d", en.Text, en.RefMessageID)
	case platform.JournalEntryUncaptured:
		return fmt.Sprintf("bot also called %s (uncaptured)", en.Method)
	default:
		who := "user"
		if en.Direction == platform.DirectionBot {
			who = "bot"
		}
		if en.Version > 0 {
			return fmt.Sprintf("[%d %s] %s (v%d, edited)", en.MessageID, who, en.Text, en.Version+1)
		}
		return fmt.Sprintf("[%d %s] %s", en.MessageID, who, en.Text)
	}
}

// actionsFromMarkup converts a Telegram inline keyboard into neutral action
// rows, shared by normalize (platform.Message) and toPlatformEntry
// (platform.JournalEntry).
func actionsFromMarkup(markup *tgbotapi.InlineKeyboardMarkup) [][]platform.Action {
	if markup == nil {
		return nil
	}
	actions := make([][]platform.Action, 0, len(markup.InlineKeyboard))
	for _, row := range markup.InlineKeyboard {
		arow := make([]platform.Action, 0, len(row))
		for _, b := range row {
			arow = append(arow, platform.Action{Label: b.Text, ID: b.CallbackData, URL: b.URL})
		}
		actions = append(actions, arow)
	}
	return actions
}

// normalize converts a journal entry into a neutral message.
func normalize(en *journalEntry) *platform.Message {
	return &platform.Message{
		Platform:   "telegram",
		ChatID:     en.chatID,
		MessageID:  en.messageID,
		Text:       en.text,
		ReceivedAt: en.at,
		Version:    en.version,
		Actions:    actionsFromMarkup(en.markup),
	}
}

// parsePath splits "/bot<token>/<method>" into token and method.
func parsePath(path string) (token, method string) {
	path = strings.TrimPrefix(path, "/")
	slash := strings.Index(path, "/")
	if slash < 0 {
		return "", strings.TrimPrefix(path, "bot")
	}
	return strings.TrimPrefix(path[:slash], "bot"), path[slash+1:]
}

// parseSendMessage extracts chat_id, text and reply_markup from a sendMessage
// request, accepting either application/json or form-urlencoded bodies. It
// returns an error only for a body that fails to parse at all (invalid JSON,
// invalid form encoding) — missing individual fields are the caller's
// business (handleSendMessage rejects those with a specific description).
func parseSendMessage(r *http.Request) (chatID int64, text string, markup *tgbotapi.InlineKeyboardMarkup, err error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var p struct {
			ChatID      json.RawMessage                `json:"chat_id"`
			Text        string                         `json:"text"`
			ReplyMarkup *tgbotapi.InlineKeyboardMarkup `json:"reply_markup"`
		}
		if err = json.NewDecoder(r.Body).Decode(&p); err != nil {
			return 0, "", nil, fmt.Errorf("sendMessage: invalid JSON body: %w", err)
		}
		return parseChatID(string(p.ChatID)), p.Text, p.ReplyMarkup, nil
	}

	if err = r.ParseForm(); err != nil {
		return 0, "", nil, fmt.Errorf("sendMessage: invalid form body: %w", err)
	}
	if rm := r.FormValue("reply_markup"); rm != "" {
		var m tgbotapi.InlineKeyboardMarkup
		if json.Unmarshal([]byte(rm), &m) == nil {
			markup = &m
		}
	}
	return parseChatID(r.FormValue("chat_id")), r.FormValue("text"), markup, nil
}

// parseEditMessageText extracts chat_id, message_id, text and reply_markup
// from an editMessageText request, accepting either application/json or
// form-urlencoded bodies — mirroring parseSendMessage. Real Telegram accepts
// JSON here too; previously only form bodies were parsed, so a non-Go bot
// (Python, Node, ...) editing via JSON silently got empty chat_id/message_id/
// text and a confusing "message to edit not found".
func parseEditMessageText(r *http.Request) (chatID int64, messageID int, text string, markup *tgbotapi.InlineKeyboardMarkup, err error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var p struct {
			ChatID      json.RawMessage                `json:"chat_id"`
			MessageID   int                            `json:"message_id"`
			Text        string                         `json:"text"`
			ReplyMarkup *tgbotapi.InlineKeyboardMarkup `json:"reply_markup"`
		}
		if err = json.NewDecoder(r.Body).Decode(&p); err != nil {
			return 0, 0, "", nil, fmt.Errorf("editMessageText: invalid JSON body: %w", err)
		}
		return parseChatID(string(p.ChatID)), p.MessageID, p.Text, p.ReplyMarkup, nil
	}

	if err = r.ParseForm(); err != nil {
		return 0, 0, "", nil, fmt.Errorf("editMessageText: invalid form body: %w", err)
	}
	messageID, _ = strconv.Atoi(r.FormValue("message_id"))
	if rm := r.FormValue("reply_markup"); rm != "" {
		var m tgbotapi.InlineKeyboardMarkup
		if json.Unmarshal([]byte(rm), &m) == nil {
			markup = &m
		}
	}
	return parseChatID(r.FormValue("chat_id")), messageID, r.FormValue("text"), markup, nil
}

// parseGenericChatID best-effort extracts a top-level chat_id field from a
// request, accepting either JSON or form-urlencoded bodies. Used only for
// methods the emulator does not otherwise understand (handleUnsupported),
// where all it needs is which chat to attribute the call to for the
// transcript — not full validation of an unknown payload shape.
func parseGenericChatID(r *http.Request) int64 {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var p struct {
			ChatID json.RawMessage `json:"chat_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		return parseChatID(string(p.ChatID))
	}
	_ = r.ParseForm()
	return parseChatID(r.FormValue("chat_id"))
}

func parseChatID(s string) int64 {
	s = strings.Trim(strings.TrimSpace(s), `"`)
	id, _ := strconv.ParseInt(s, 10, 64)
	return id
}

// writeResult writes a Bot API envelope {"ok":true,"result":<result>}.
func writeResult(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

// writeError writes a Bot API error envelope {"ok":false,"error_code":400,
// "description":<msg>} with a matching HTTP 400 status — a malformed or
// incomplete request (bad JSON, a missing required field, editing a message
// that doesn't exist) is a real Bot API 400, not a silently-accepted no-op.
func writeError(w http.ResponseWriter, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": description})
}

// writeUnsupported writes the Bot API error envelope for a method the
// emulator does not simulate: {"ok":false,"error_code":501,"description":
// "method not emulated: <method>"}, with a matching HTTP 501 status. 501 is
// not a code the real Bot API would ever return — an unimplemented method
// there is simply unreachable — but it usefully distinguishes "chatwright
// doesn't emulate this yet" from a genuine validation failure (400).
func writeUnsupported(w http.ResponseWriter, method string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":          false,
		"error_code":  501,
		"description": "method not emulated: " + method,
	})
}

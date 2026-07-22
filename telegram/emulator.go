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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bots-go-framework/bots-api-telegram/tgbotapi"

	"github.com/chatwright/chatwright/platform"
)

// Platform returns the Telegram platform for use with chatwright.OnPlatform.
func Platform() platform.Platform { return tgPlatform{} }

type tgPlatform struct{}

func (tgPlatform) Name() string { return "telegram" }

func (tgPlatform) Start() platform.Emulator { return NewEmulator() }

// journalDirection distinguishes who produced a journal entry.
type journalDirection int

const (
	fromUser journalDirection = iota
	fromBot
)

// journalKind distinguishes the shape of a journal entry.
type journalKind int

const (
	kindText     journalKind = iota // an inbound message or an outbound sendMessage/editMessageText
	kindCallback                    // an inbound button click
)

// journalEntry is one immutable entry in a chat's append-only event journal.
// A message edit never mutates a prior entry: it appends a new entry carrying
// the same messageID and an incremented version, so intermediate content and
// the true call order both survive. The "current" state of a messageID is its
// highest-version entry, derived by callers rather than stored separately.
type journalEntry struct {
	chatID       int64
	dir          journalDirection
	kind         journalKind
	messageID    int // own identity, shared by inbound and outbound messages in this chat; 0 for entries with no message identity of their own (callbacks)
	refMessageID int // kindCallback only: the bot message the click targeted
	version      int // 0 = original send/inbound; N = the Nth edit of this messageID
	text         string
	markup       *tgbotapi.InlineKeyboardMarkup
	at           time.Time
}

// Emulator is an in-process HTTP server emulating the Telegram Bot API.
type Emulator struct {
	server *httptest.Server

	mu        sync.Mutex
	journal   []journalEntry
	nextMsgID map[int64]int // per-chat shared message-ID sequence, inbound and outbound alike
	updated   chan struct{} // closed (and replaced) on every new journal entry; broadcast signal
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

// BotAPIURL is the base URL the bot-under-test should use as its Telegram Bot API
// host, in place of https://api.telegram.org.
func (e *Emulator) BotAPIURL() string { return e.server.URL }

// Close shuts down the emulator's HTTP server.
func (e *Emulator) Close() { e.server.Close() }

// reserveMessageIDLocked returns the next ID in chatID's shared message-ID
// sequence, starting at 1. Caller must hold e.mu.
func (e *Emulator) reserveMessageIDLocked(chatID int64) int {
	e.nextMsgID[chatID]++
	return e.nextMsgID[chatID]
}

// appendLocked appends entry to the journal and wakes any waiters. Caller must
// hold e.mu.
func (e *Emulator) appendLocked(entry journalEntry) {
	e.journal = append(e.journal, entry)
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

// RecordInboundText reserves the next message ID in chatID's shared sequence
// and appends an inbound journal entry for it.
func (e *Emulator) RecordInboundText(chatID int64, user platform.User, text string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := e.reserveMessageIDLocked(chatID)
	e.appendLocked(journalEntry{chatID: chatID, dir: fromUser, kind: kindText, messageID: id, text: text, at: time.Now()})
	return id
}

// RecordInboundCallback appends a journal entry for a button click. It does
// not reserve a message ID: a callback query references an existing message
// rather than creating a new one.
func (e *Emulator) RecordInboundCallback(chatID int64, user platform.User, data string, targetMessageID int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.appendLocked(journalEntry{chatID: chatID, dir: fromUser, kind: kindCallback, refMessageID: targetMessageID, text: data, at: time.Now()})
}

// EncodeInboundText builds a Telegram update carrying a user's text message.
func (e *Emulator) EncodeInboundText(in platform.Inbound) (string, []byte) {
	update := tgbotapi.Update{
		UpdateID: in.UpdateID,
		Message: &tgbotapi.Message{
			MessageID: in.MessageID,
			From: &tgbotapi.User{
				ID:        in.User.ID,
				FirstName: in.User.FirstName,
				LastName:  in.User.LastName,
				UserName:  in.User.Username,
			},
			Chat: &tgbotapi.Chat{ID: in.ChatID, Type: "private", FirstName: in.User.FirstName},
			Date: int(time.Now().Unix()),
			Text: in.Text,
		},
	}
	body, _ := json.Marshal(update)
	return "application/json", body
}

// EncodeCallback builds a Telegram callback query (an inline-button click).
func (e *Emulator) EncodeCallback(in platform.InboundCallback) (string, []byte) {
	from := &tgbotapi.User{
		ID:        in.User.ID,
		FirstName: in.User.FirstName,
		LastName:  in.User.LastName,
		UserName:  in.User.Username,
	}
	update := tgbotapi.Update{
		UpdateID: in.UpdateID,
		CallbackQuery: &tgbotapi.CallbackQuery{
			ID:   in.CallbackID,
			From: from,
			Data: in.Data,
			Message: &tgbotapi.Message{
				MessageID: in.MessageID,
				From:      &tgbotapi.User{ID: 1, IsBot: true, FirstName: "ChatwrightBot"},
				Chat:      &tgbotapi.Chat{ID: in.ChatID, Type: "private", FirstName: in.User.FirstName},
			},
		},
	}
	body, _ := json.Marshal(update)
	return "application/json", body
}

// handle routes /bot<token>/<method> like the real Bot API.
func (e *Emulator) handle(w http.ResponseWriter, r *http.Request) {
	_, method := parsePath(r.URL.Path)

	switch method {
	case "getMe":
		writeResult(w, tgbotapi.User{ID: 1, IsBot: true, FirstName: "ChatwrightBot", UserName: "chatwright_bot"})
	case "sendMessage":
		e.handleSendMessage(w, r)
	case "editMessageText":
		e.handleEditMessageText(w, r)
	default:
		// Be lenient: acknowledge any other method (setWebhook, deleteWebhook,
		// answerCallbackQuery, setMyCommands, ...) so arbitrary bots don't error.
		writeResult(w, true)
	}
}

func (e *Emulator) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	chatID, text, markup := parseSendMessage(r)

	e.mu.Lock()
	messageID := e.reserveMessageIDLocked(chatID)
	at := time.Now()
	e.appendLocked(journalEntry{chatID: chatID, dir: fromBot, kind: kindText, messageID: messageID, text: text, markup: markup, at: at})
	e.mu.Unlock()

	writeResult(w, tgbotapi.Message{
		MessageID: messageID,
		From:      &tgbotapi.User{ID: 1, IsBot: true, FirstName: "ChatwrightBot"},
		Chat:      &tgbotapi.Chat{ID: chatID, Type: "private"},
		Date:      int(at.Unix()),
		Text:      text,
	})
}

// handleEditMessageText emulates editMessageText: it appends a new, versioned
// journal entry for the identified message (rather than mutating its prior
// entry), so WaitForEdit can observe the change and the transcript can show
// the message's full edit history was recorded, even though only its current
// state is displayed.
func (e *Emulator) handleEditMessageText(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	chatID := parseChatID(r.FormValue("chat_id"))
	messageID, _ := strconv.Atoi(r.FormValue("message_id"))
	text := r.FormValue("text")
	var markup *tgbotapi.InlineKeyboardMarkup
	if rm := r.FormValue("reply_markup"); rm != "" {
		var m tgbotapi.InlineKeyboardMarkup
		if json.Unmarshal([]byte(rm), &m) == nil {
			markup = &m
		}
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
	})
	e.mu.Unlock()

	writeResult(w, tgbotapi.Message{
		MessageID: messageID,
		From:      &tgbotapi.User{ID: 1, IsBot: true, FirstName: "ChatwrightBot"},
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
// of what any BotMessage handle has consumed or asserted on.
func (e *Emulator) Transcript(chatID int64) string {
	e.mu.Lock()
	defer e.mu.Unlock()

	var lines []string
	posByID := map[int]int{}
	for _, en := range e.journal {
		if en.chatID != chatID {
			continue
		}
		if en.kind == kindText {
			if i, ok := posByID[en.messageID]; ok {
				lines[i] = renderEntry(en) // an edit: update this message's line in place, no new line
				continue
			}
			posByID[en.messageID] = len(lines)
		}
		lines = append(lines, renderEntry(en))
	}

	if len(lines) == 0 {
		return fmt.Sprintf("chat %d transcript: (empty — no messages yet)", chatID)
	}
	return fmt.Sprintf("chat %d transcript: %s", chatID, strings.Join(lines, " / "))
}

// renderEntry renders one transcript line for en.
func renderEntry(en journalEntry) string {
	if en.kind == kindCallback {
		return fmt.Sprintf("[user] clicked %q on message %d", en.text, en.refMessageID)
	}
	who := "user"
	if en.dir == fromBot {
		who = "bot"
	}
	if en.version > 0 {
		return fmt.Sprintf("[%d %s] %s (v%d, edited)", en.messageID, who, en.text, en.version+1)
	}
	return fmt.Sprintf("[%d %s] %s", en.messageID, who, en.text)
}

// normalize converts a journal entry into a neutral message.
func normalize(en *journalEntry) *platform.Message {
	m := &platform.Message{
		Platform:   "telegram",
		ChatID:     en.chatID,
		MessageID:  en.messageID,
		Text:       en.text,
		ReceivedAt: en.at,
		Version:    en.version,
	}
	if en.markup != nil {
		for _, row := range en.markup.InlineKeyboard {
			arow := make([]platform.Action, 0, len(row))
			for _, b := range row {
				arow = append(arow, platform.Action{Label: b.Text, ID: b.CallbackData, URL: b.URL})
			}
			m.Actions = append(m.Actions, arow)
		}
	}
	return m
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
// request, accepting either application/json or form-urlencoded bodies.
func parseSendMessage(r *http.Request) (chatID int64, text string, markup *tgbotapi.InlineKeyboardMarkup) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var p struct {
			ChatID      json.RawMessage                `json:"chat_id"`
			Text        string                         `json:"text"`
			ReplyMarkup *tgbotapi.InlineKeyboardMarkup `json:"reply_markup"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		return parseChatID(string(p.ChatID)), p.Text, p.ReplyMarkup
	}

	_ = r.ParseForm()
	if rm := r.FormValue("reply_markup"); rm != "" {
		var m tgbotapi.InlineKeyboardMarkup
		if json.Unmarshal([]byte(rm), &m) == nil {
			markup = &m
		}
	}
	return parseChatID(r.FormValue("chat_id")), r.FormValue("text"), markup
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

// writeError writes a Bot API error envelope {"ok":false,"description":<msg>}.
func writeError(w http.ResponseWriter, description string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": description})
}

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
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
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

var _ platform.InlineQueryEmulator = (*Emulator)(nil)
var _ platform.ChosenInlineResultEmulator = (*Emulator)(nil)

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

	pendingInlineQueries map[string]inlineQueryState
	inlineAnswers        map[string]platform.InlineQueryAnswer
	selectedInline       map[string]selectedInlineState
}

type inlineQueryState struct {
	user  platform.User
	query string
}

type selectedInlineState struct {
	result  platform.InlineQueryResult
	version int
}

// NewEmulator starts a fake Telegram Bot API server on a random local port.
func NewEmulator() *Emulator {
	e := &Emulator{
		nextMsgID:            make(map[int64]int),
		updated:              make(chan struct{}),
		pendingInlineQueries: make(map[string]inlineQueryState),
		inlineAnswers:        make(map[string]platform.InlineQueryAnswer),
		selectedInline:       make(map[string]selectedInlineState),
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
		nextMsgID:            make(map[int64]int),
		updated:              make(chan struct{}),
		pendingInlineQueries: make(map[string]inlineQueryState),
		inlineAnswers:        make(map[string]platform.InlineQueryAnswer),
		selectedInline:       make(map[string]selectedInlineState),
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
				ID:           user.ID,
				FirstName:    user.FirstName,
				LastName:     user.LastName,
				UserName:     user.Username,
				LanguageCode: user.LanguageCode,
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
				ID:           user.ID,
				FirstName:    user.FirstName,
				LastName:     user.LastName,
				UserName:     user.Username,
				LanguageCode: user.LanguageCode,
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

// SubmitInlineQuery delivers Telegram's chat-independent inline_query update.
// It deliberately does not journal a chat message: opening inline mode and
// receiving results do not post anything to the selected chat.
func (e *Emulator) SubmitInlineQuery(user platform.User, query, offset string) (string, error) {
	e.mu.Lock()
	updateID := e.reserveUpdateIDLocked()
	queryID := "iq" + strconv.Itoa(updateID)
	e.pendingInlineQueries[queryID] = inlineQueryState{user: user, query: query}
	webhookURL, client := e.webhookURL, e.httpClient
	e.mu.Unlock()

	update := tgbotapi.Update{
		UpdateID: updateID,
		InlineQuery: &tgbotapi.InlineQuery{
			ID: queryID,
			From: &tgbotapi.User{
				ID:           user.ID,
				FirstName:    user.FirstName,
				LastName:     user.LastName,
				UserName:     user.Username,
				LanguageCode: user.LanguageCode,
			},
			Query:  query,
			Offset: offset,
		},
	}
	if err := e.deliver(update, webhookURL, client); err != nil {
		return queryID, err
	}
	return queryID, nil
}

// WaitForInlineQueryAnswer waits for the bot to answer queryID.
func (e *Emulator) WaitForInlineQueryAnswer(queryID string, timeout time.Duration) (*platform.InlineQueryAnswer, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		e.mu.Lock()
		if answer, ok := e.inlineAnswers[queryID]; ok {
			copy := answer
			e.mu.Unlock()
			return &copy, true
		}
		ch := e.updated
		e.mu.Unlock()

		select {
		case <-ch:
		case <-timer.C:
			return nil, false
		}
	}
}

func (e *Emulator) SelectInlineQueryResult(
	user platform.User,
	queryID, resultID string,
) (string, error) {
	e.mu.Lock()
	query, found := e.pendingInlineQueries[queryID]
	answer, answered := e.inlineAnswers[queryID]
	if !found || !answered {
		e.mu.Unlock()
		return "", fmt.Errorf("inline query %q is not answered", queryID)
	}
	var selected platform.InlineQueryResult
	for _, result := range answer.Results {
		if result.ID == resultID {
			selected = result
			break
		}
	}
	if selected.ID == "" {
		e.mu.Unlock()
		return "", fmt.Errorf("inline result %q was not returned for %q", resultID, queryID)
	}
	if user.ID == 0 {
		user = query.user
	}
	updateID := e.reserveUpdateIDLocked()
	inlineMessageID := fmt.Sprintf("inline-%s-%d", queryID, updateID)
	e.selectedInline[inlineMessageID] = selectedInlineState{result: selected}
	webhookURL, client := e.webhookURL, e.httpClient
	e.mu.Unlock()

	update := tgbotapi.Update{
		UpdateID: updateID,
		ChosenInlineResult: &tgbotapi.ChosenInlineResult{
			ResultID: resultID,
			From: &tgbotapi.User{
				ID:           user.ID,
				FirstName:    user.FirstName,
				LastName:     user.LastName,
				UserName:     user.Username,
				LanguageCode: user.LanguageCode,
			},
			InlineMessageID: inlineMessageID,
			Query:           query.query,
		},
	}
	if err := e.deliver(update, webhookURL, client); err != nil {
		return "", err
	}
	return inlineMessageID, nil
}

func (e *Emulator) WaitForInlineResultEdit(
	inlineMessageID string,
	afterVersion int,
	timeout time.Duration,
) (*platform.InlineQueryResult, int, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		e.mu.Lock()
		state, ok := e.selectedInline[inlineMessageID]
		if ok && state.version > afterVersion {
			result := state.result
			e.mu.Unlock()
			return &result, state.version, true
		}
		ch := e.updated
		e.mu.Unlock()
		select {
		case <-ch:
		case <-timer.C:
			return nil, 0, false
		}
	}
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
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		detail := strings.TrimSpace(string(responseBody))
		if detail == "" {
			return fmt.Errorf("chatwright: webhook returned status %d", resp.StatusCode)
		}
		return fmt.Errorf("chatwright: webhook returned status %d: %s", resp.StatusCode, detail)
	}
	if err := e.captureInlineWebhookResponse(responseBody, resp.Header.Get("Content-Type")); err != nil {
		return err
	}
	return nil
}

// captureInlineWebhookResponse emulates Telegram's ability to execute one Bot
// API method by returning its JSON payload directly as the webhook response.
// Frameworks use this latency-saving path for simple replies; ignoring it
// makes a correctly replying bot appear silent to a black-box scenario.
func (e *Emulator) captureInlineWebhookResponse(body []byte, contentType string) error {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil
	}
	var method string
	isJSON := strings.HasPrefix(contentType, "application/json") || body[0] == '{'
	if isJSON {
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			return fmt.Errorf("chatwright: webhook returned invalid inline Bot API JSON: %w", err)
		}
		method, _ = payload["method"].(string)
	} else {
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return fmt.Errorf("chatwright: webhook returned invalid inline Bot API form: %w", err)
		}
		method = values.Get("method")
		contentType = "application/x-www-form-urlencoded"
	}
	if method == "" {
		// A plain acknowledgement body isn't an inline Bot API request.
		return nil
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/botINLINE/"+method,
		bytes.NewReader(body),
	)
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	e.handle(response, request)
	if response.Code >= http.StatusBadRequest {
		detail := strings.TrimSpace(response.Body.String())
		return fmt.Errorf(
			"chatwright: inline webhook Bot API method %s failed with status %d: %s",
			method,
			response.Code,
			detail,
		)
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
	case "sendRichMessage":
		e.handleSendRichMessage(w, r, token)
	case "sendRichMessageDraft":
		e.handleSendRichMessageDraft(w, r)
	case "editMessageText":
		e.handleEditMessageText(w, r, token)
	case "editMessageCaption":
		e.handleEditMessageCaption(w, r)
	case "answerInlineQuery":
		e.handleAnswerInlineQuery(w, r)
	default:
		if acknowledgedMethods[method] {
			writeResult(w, true)
			return
		}
		e.handleUnsupported(w, r, token, method)
	}
}

func (e *Emulator) handleEditMessageCaption(w http.ResponseWriter, r *http.Request) {
	inlineMessageID, caption, markup, err := parseEditMessageCaption(r)
	if err != nil {
		writeError(w, err.Error())
		return
	}
	if inlineMessageID == "" {
		writeError(w, "editMessageCaption: inline_message_id is required")
		return
	}
	e.mu.Lock()
	state, ok := e.selectedInline[inlineMessageID]
	if !ok {
		e.mu.Unlock()
		writeError(w, "editMessageCaption: inline message not found")
		return
	}
	state.result.Caption = caption
	if markup != nil {
		state.result.Actions = actionsFromMarkup(markup)
	}
	state.version++
	e.selectedInline[inlineMessageID] = state
	e.wakeLocked()
	e.mu.Unlock()
	writeResult(w, true)
}

func (e *Emulator) handleAnswerInlineQuery(w http.ResponseWriter, r *http.Request) {
	answer, err := parseInlineQueryAnswer(r)
	if err != nil {
		writeError(w, err.Error())
		return
	}
	if answer.QueryID == "" {
		writeError(w, "answerInlineQuery: inline_query_id is required")
		return
	}
	if len(answer.Results) == 0 {
		writeError(w, "answerInlineQuery: results are required")
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.pendingInlineQueries[answer.QueryID]; !ok {
		writeError(w, "answerInlineQuery: inline query not found")
		return
	}
	if _, answered := e.inlineAnswers[answer.QueryID]; answered {
		writeError(w, "answerInlineQuery: inline query was already answered")
		return
	}
	e.inlineAnswers[answer.QueryID] = answer
	e.wakeLocked()
	writeResult(w, true)
}

// handleSendRichMessage emulates Bot API 10.1/10.2 native rich messages. The
// exact InputRichMessage remains Telegram-native at the wire seam; Chatwright's
// neutral transcript gets a deterministic plain-text projection so existing
// message assertions and accessibility checks continue to work.
func (e *Emulator) handleSendRichMessage(w http.ResponseWriter, r *http.Request, token string) {
	chatID, rich, markup, err := parseRichMessageRequest(r, "sendRichMessage")
	if err != nil {
		writeError(w, err.Error())
		return
	}
	if chatID == 0 {
		writeError(w, "sendRichMessage: chat_id is required")
		return
	}
	text := richMessageText(rich)
	if text == "" {
		writeError(w, "sendRichMessage: rich_message is required")
		return
	}

	e.mu.Lock()
	messageID := e.reserveMessageIDLocked(chatID)
	at := time.Now()
	e.appendLocked(journalEntry{
		chatID: chatID, dir: fromBot, kind: kindText, messageID: messageID,
		text: text, markup: markup, at: at, method: "sendRichMessage",
		token: token, fromID: EmulatedBotUserID,
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

// handleSendRichMessageDraft validates and acknowledges streamed draft
// updates. Drafts are temporary previews rather than persistent chat messages,
// so they intentionally do not advance the message expectation cursor.
func (e *Emulator) handleSendRichMessageDraft(w http.ResponseWriter, r *http.Request) {
	chatID, rich, _, err := parseRichMessageRequest(r, "sendRichMessageDraft")
	if err != nil {
		writeError(w, err.Error())
		return
	}
	if chatID == 0 || richMessageText(rich) == "" {
		writeError(w, "sendRichMessageDraft: chat_id and rich_message are required")
		return
	}
	writeResult(w, true)
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
	chatID, messageID, text, rich, markup, err := parseEditMessageText(r)
	if err != nil {
		writeError(w, err.Error())
		return
	}
	if chatID == 0 || messageID == 0 {
		writeError(w, "editMessageText: chat_id and message_id are required")
		return
	}
	if rich != nil {
		text = richMessageText(*rich)
	}
	if text == "" {
		writeError(w, "editMessageText: text or rich_message is required")
		return
	}

	e.mu.Lock()
	prev, found := e.latestTextEntryLocked(chatID, messageID)
	if !found {
		e.mu.Unlock()
		writeError(w, "message to edit not found")
		return
	}
	// editMessageText without reply_markup REMOVES the existing keyboard —
	// real Telegram only keeps a message's inline keyboard when the edit
	// call explicitly re-sends reply_markup; omitting it clears the keyboard
	// (decision 0015, cross-repo parity register docs/runtime-parity.md).
	// markup is already nil here in that case, so no fallback to prev.markup.
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
			action := platform.Action{Label: b.Text, ID: b.CallbackData, URL: b.URL}
			if b.CopyText != nil {
				action.CopyText = b.CopyText.Text
			}
			switch {
			case b.SwitchInlineQueryChosenChat != nil:
				action.OpensInlineQuery = true
				action.InlineQuery = b.SwitchInlineQueryChosenChat.Query
			case b.SwitchInlineQuery != nil:
				action.OpensInlineQuery = true
				action.InlineQuery = *b.SwitchInlineQuery
			case b.SwitchInlineQueryCurrentChat != nil:
				action.OpensInlineQuery = true
				action.InlineQuery = *b.SwitchInlineQueryCurrentChat
			}
			arow = append(arow, action)
		}
		actions = append(actions, arow)
	}
	return actions
}

type inlineQueryResultWire struct {
	Type                string                         `json:"type"`
	ID                  string                         `json:"id"`
	Title               string                         `json:"title,omitempty"`
	Description         string                         `json:"description,omitempty"`
	PhotoURL            string                         `json:"photo_url,omitempty"`
	ThumbnailURL        string                         `json:"thumbnail_url,omitempty"`
	LegacyThumbnailURL  string                         `json:"thumb_url,omitempty"`
	Caption             string                         `json:"caption,omitempty"`
	InputMessageContent json.RawMessage                `json:"input_message_content,omitempty"`
	ReplyMarkup         *tgbotapi.InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

type inputMessageContentWire struct {
	MessageText string                `json:"message_text,omitempty"`
	RichMessage *inputRichMessageWire `json:"rich_message,omitempty"`
}

func parseInlineQueryAnswer(r *http.Request) (platform.InlineQueryAnswer, error) {
	var answer platform.InlineQueryAnswer
	var resultsJSON []byte
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var payload struct {
			QueryID    string          `json:"inline_query_id"`
			Results    json.RawMessage `json:"results"`
			CacheTime  int             `json:"cache_time"`
			IsPersonal bool            `json:"is_personal"`
			NextOffset string          `json:"next_offset"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			return answer, fmt.Errorf("answerInlineQuery: invalid JSON body: %w", err)
		}
		answer.QueryID = payload.QueryID
		answer.CacheTime = payload.CacheTime
		answer.IsPersonal = payload.IsPersonal
		answer.NextOffset = payload.NextOffset
		resultsJSON = payload.Results
	} else {
		if err := r.ParseForm(); err != nil {
			return answer, fmt.Errorf("answerInlineQuery: invalid form body: %w", err)
		}
		answer.QueryID = r.FormValue("inline_query_id")
		answer.CacheTime, _ = strconv.Atoi(r.FormValue("cache_time"))
		answer.IsPersonal, _ = strconv.ParseBool(r.FormValue("is_personal"))
		answer.NextOffset = r.FormValue("next_offset")
		resultsJSON = []byte(r.FormValue("results"))
	}
	if len(bytes.TrimSpace(resultsJSON)) == 0 {
		return answer, errors.New("answerInlineQuery: results are required")
	}

	var wireResults []inlineQueryResultWire
	if err := json.Unmarshal(resultsJSON, &wireResults); err != nil {
		return answer, fmt.Errorf("answerInlineQuery: invalid results: %w", err)
	}
	answer.Results = make([]platform.InlineQueryResult, len(wireResults))
	for i, wire := range wireResults {
		if wire.Type == "" {
			return answer, fmt.Errorf("answerInlineQuery: result %d type is required", i)
		}
		if wire.ID == "" {
			return answer, fmt.Errorf("answerInlineQuery: result %d id is required", i)
		}
		if wire.Type == "photo" && wire.PhotoURL == "" {
			return answer, fmt.Errorf("answerInlineQuery: photo result %d photo_url is required", i)
		}
		result := platform.InlineQueryResult{
			Type:         wire.Type,
			ID:           wire.ID,
			Title:        wire.Title,
			Description:  wire.Description,
			PhotoURL:     wire.PhotoURL,
			ThumbnailURL: wire.ThumbnailURL,
			Caption:      wire.Caption,
			Actions:      actionsFromMarkup(wire.ReplyMarkup),
		}
		if result.ThumbnailURL == "" {
			result.ThumbnailURL = wire.LegacyThumbnailURL
		}
		if len(wire.InputMessageContent) > 0 {
			var content inputMessageContentWire
			if err := json.Unmarshal(wire.InputMessageContent, &content); err != nil {
				return answer, fmt.Errorf("answerInlineQuery: invalid input_message_content for result %d: %w", i, err)
			}
			result.Text = content.MessageText
			if result.Text == "" && content.RichMessage != nil {
				result.Text = richMessageText(*content.RichMessage)
			}
		}
		answer.Results[i] = result
	}
	return answer, nil
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
func parseEditMessageText(r *http.Request) (chatID int64, messageID int, text string, rich *inputRichMessageWire, markup *tgbotapi.InlineKeyboardMarkup, err error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var p struct {
			ChatID      json.RawMessage                `json:"chat_id"`
			MessageID   int                            `json:"message_id"`
			Text        string                         `json:"text"`
			RichMessage *inputRichMessageWire          `json:"rich_message"`
			ReplyMarkup *tgbotapi.InlineKeyboardMarkup `json:"reply_markup"`
		}
		if err = json.NewDecoder(r.Body).Decode(&p); err != nil {
			return 0, 0, "", nil, nil, fmt.Errorf("editMessageText: invalid JSON body: %w", err)
		}
		if p.RichMessage != nil {
			if err = validateInputRichMessage(*p.RichMessage); err != nil {
				return 0, 0, "", nil, nil, fmt.Errorf("editMessageText: invalid rich_message: %w", err)
			}
		}
		return parseChatID(string(p.ChatID)), p.MessageID, p.Text, p.RichMessage, p.ReplyMarkup, nil
	}

	if err = r.ParseForm(); err != nil {
		return 0, 0, "", nil, nil, fmt.Errorf("editMessageText: invalid form body: %w", err)
	}
	messageID, _ = strconv.Atoi(r.FormValue("message_id"))
	if raw := r.FormValue("rich_message"); raw != "" {
		var value inputRichMessageWire
		if err = json.Unmarshal([]byte(raw), &value); err != nil {
			return 0, 0, "", nil, nil, fmt.Errorf("editMessageText: invalid rich_message: %w", err)
		}
		if err = validateInputRichMessage(value); err != nil {
			return 0, 0, "", nil, nil, fmt.Errorf("editMessageText: invalid rich_message: %w", err)
		}
		rich = &value
	}
	if rm := r.FormValue("reply_markup"); rm != "" {
		var m tgbotapi.InlineKeyboardMarkup
		if json.Unmarshal([]byte(rm), &m) == nil {
			markup = &m
		}
	}
	return parseChatID(r.FormValue("chat_id")), messageID, r.FormValue("text"), rich, markup, nil
}

func parseEditMessageCaption(
	r *http.Request,
) (inlineMessageID, caption string, markup *tgbotapi.InlineKeyboardMarkup, err error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var payload struct {
			InlineMessageID string                         `json:"inline_message_id"`
			Caption         string                         `json:"caption"`
			ReplyMarkup     *tgbotapi.InlineKeyboardMarkup `json:"reply_markup"`
		}
		if err = json.NewDecoder(r.Body).Decode(&payload); err != nil {
			return "", "", nil, fmt.Errorf("editMessageCaption: invalid JSON body: %w", err)
		}
		return payload.InlineMessageID, payload.Caption, payload.ReplyMarkup, nil
	}
	if err = r.ParseForm(); err != nil {
		return "", "", nil, fmt.Errorf("editMessageCaption: invalid form body: %w", err)
	}
	if raw := r.FormValue("reply_markup"); raw != "" {
		var value tgbotapi.InlineKeyboardMarkup
		if err = json.Unmarshal([]byte(raw), &value); err != nil {
			return "", "", nil, fmt.Errorf("editMessageCaption: invalid reply_markup: %w", err)
		}
		markup = &value
	}
	return r.FormValue("inline_message_id"), r.FormValue("caption"), markup, nil
}

func parseRichMessageRequest(
	r *http.Request,
	method string,
) (chatID int64, rich inputRichMessageWire, markup *tgbotapi.InlineKeyboardMarkup, err error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var p struct {
			ChatID      json.RawMessage                `json:"chat_id"`
			RichMessage inputRichMessageWire           `json:"rich_message"`
			ReplyMarkup *tgbotapi.InlineKeyboardMarkup `json:"reply_markup"`
		}
		if err = json.NewDecoder(r.Body).Decode(&p); err != nil {
			return 0, rich, nil, fmt.Errorf("%s: invalid JSON body: %w", method, err)
		}
		if err = validateInputRichMessage(p.RichMessage); err != nil {
			return 0, rich, nil, fmt.Errorf("%s: invalid rich_message: %w", method, err)
		}
		return parseChatID(string(p.ChatID)), p.RichMessage, p.ReplyMarkup, nil
	}
	if err = r.ParseForm(); err != nil {
		return 0, rich, nil, fmt.Errorf("%s: invalid form body: %w", method, err)
	}
	raw := r.FormValue("rich_message")
	if raw != "" {
		if err = json.Unmarshal([]byte(raw), &rich); err != nil {
			return 0, rich, nil, fmt.Errorf("%s: invalid rich_message: %w", method, err)
		}
		if err = validateInputRichMessage(rich); err != nil {
			return 0, rich, nil, fmt.Errorf("%s: invalid rich_message: %w", method, err)
		}
	}
	if rm := r.FormValue("reply_markup"); rm != "" {
		var value tgbotapi.InlineKeyboardMarkup
		if json.Unmarshal([]byte(rm), &value) == nil {
			markup = &value
		}
	}
	return parseChatID(r.FormValue("chat_id")), rich, markup, nil
}

// The Bot API package deliberately models InputRichBlock as an outgoing-only
// type, so its caption field is projected only while marshaling and cannot be
// recovered by unmarshaling a Bot API request. Chatwright is on the receiving
// side of that request and therefore keeps a small wire-facing projection type
// that retains captions and the recursively visible rich-message content.
type inputRichMessageWire struct {
	Blocks   []inputRichBlockWire `json:"blocks,omitempty"`
	HTML     string               `json:"html,omitempty"`
	Markdown string               `json:"markdown,omitempty"`
}

type inputRichBlockWire struct {
	Type       string                   `json:"type,omitempty"`
	Text       json.RawMessage          `json:"text,omitempty"`
	Summary    json.RawMessage          `json:"summary,omitempty"`
	Caption    json.RawMessage          `json:"caption,omitempty"`
	Credit     json.RawMessage          `json:"credit,omitempty"`
	Expression string                   `json:"expression,omitempty"`
	Blocks     []inputRichBlockWire     `json:"blocks,omitempty"`
	Items      []inputRichBlockListWire `json:"items,omitempty"`
	Cells      [][]richBlockCellWire    `json:"cells,omitempty"`
}

type inputRichBlockListWire struct {
	Blocks      []inputRichBlockWire `json:"blocks,omitempty"`
	HasCheckbox bool                 `json:"has_checkbox,omitempty"`
	IsChecked   bool                 `json:"is_checked,omitempty"`
}

type richBlockCellWire struct {
	Text json.RawMessage `json:"text,omitempty"`
}

var telegramDateTimeFormatPattern = regexp.MustCompile(`^(r|w?[dD]?[tT]?)$`)

func validateInputRichMessage(rich inputRichMessageWire) error {
	return validateRichBlocks(rich.Blocks)
}

func validateRichBlocks(blocks []inputRichBlockWire) error {
	for blockIndex := range blocks {
		block := &blocks[blockIndex]
		for _, field := range []struct {
			name string
			text json.RawMessage
		}{
			{name: "text", text: block.Text},
			{name: "summary", text: block.Summary},
			{name: "caption", text: block.Caption},
			{name: "credit", text: block.Credit},
		} {
			if err := validateRichText(field.text); err != nil {
				return fmt.Errorf("blocks[%d].%s: %w", blockIndex, field.name, err)
			}
		}
		for rowIndex := range block.Cells {
			for cellIndex := range block.Cells[rowIndex] {
				if err := validateRichText(block.Cells[rowIndex][cellIndex].Text); err != nil {
					return fmt.Errorf("blocks[%d].cells[%d][%d].text: %w", blockIndex, rowIndex, cellIndex, err)
				}
			}
		}
		for itemIndex := range block.Items {
			if err := validateRichBlocks(block.Items[itemIndex].Blocks); err != nil {
				return fmt.Errorf("blocks[%d].items[%d]: %w", blockIndex, itemIndex, err)
			}
		}
		if err := validateRichBlocks(block.Blocks); err != nil {
			return fmt.Errorf("blocks[%d]: %w", blockIndex, err)
		}
	}
	return nil
}

func validateRichText(raw json.RawMessage) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	switch raw[0] {
	case '"':
		return nil
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return fmt.Errorf("invalid rich text array: %w", err)
		}
		for itemIndex := range items {
			if err := validateRichText(items[itemIndex]); err != nil {
				return fmt.Errorf("items[%d]: %w", itemIndex, err)
			}
		}
		return nil
	case '{':
		var value struct {
			Type           string          `json:"type"`
			Text           json.RawMessage `json:"text"`
			Credit         json.RawMessage `json:"credit"`
			UnixTime       *int64          `json:"unix_time"`
			DateTimeFormat *string         `json:"date_time_format"`
		}
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("invalid rich text object: %w", err)
		}
		if value.Type == tgbotapi.RichTextTypeDateTime {
			if value.UnixTime == nil {
				return fmt.Errorf("unix_time is required for date_time rich text")
			}
			if value.DateTimeFormat == nil {
				return fmt.Errorf("date_time_format is required for date_time rich text")
			}
			if !telegramDateTimeFormatPattern.MatchString(*value.DateTimeFormat) {
				return fmt.Errorf(
					"date_time_format %q must match r|w?[dD]?[tT]?",
					*value.DateTimeFormat,
				)
			}
		}
		if err := validateRichText(value.Text); err != nil {
			return fmt.Errorf("text: %w", err)
		}
		if err := validateRichText(value.Credit); err != nil {
			return fmt.Errorf("credit: %w", err)
		}
		return nil
	default:
		return nil
	}
}

func richMessageText(rich inputRichMessageWire) string {
	switch {
	case rich.HTML != "":
		return strings.TrimSpace(stripHTML(rich.HTML))
	case rich.Markdown != "":
		return strings.TrimSpace(rich.Markdown)
	default:
		lines := make([]string, 0, len(rich.Blocks))
		appendRichBlocksText(&lines, rich.Blocks, "")
		return strings.TrimSpace(strings.Join(lines, "\n"))
	}
}

func appendRichBlocksText(lines *[]string, blocks []inputRichBlockWire, prefix string) {
	for _, block := range blocks {
		switch block.Type {
		case tgbotapi.RichBlockTypeDivider:
			*lines = append(*lines, "—")
		case tgbotapi.RichBlockTypeTable:
			if caption := richTextJSONValue(block.Caption); caption != "" {
				*lines = append(*lines, prefix+caption)
			}
			for _, row := range block.Cells {
				cells := make([]string, 0, len(row))
				for _, cell := range row {
					cells = append(cells, richTextJSONValue(cell.Text))
				}
				*lines = append(*lines, strings.Join(cells, " | "))
			}
		case tgbotapi.RichBlockTypeDetails:
			if summary := richTextJSONValue(block.Summary); summary != "" {
				*lines = append(*lines, prefix+summary)
			}
			appendRichBlocksText(lines, block.Blocks, prefix)
		case tgbotapi.RichBlockTypeList:
			for _, item := range block.Items {
				before := len(*lines)
				itemPrefix := "• "
				if item.HasCheckbox {
					if item.IsChecked {
						itemPrefix = "☑ "
					} else {
						itemPrefix = "☐ "
					}
				}
				appendRichBlocksText(lines, item.Blocks, itemPrefix)
				if len(*lines) == before {
					*lines = append(*lines, strings.TrimSpace(itemPrefix))
				}
			}
		default:
			if text := richTextJSONValue(block.Text); text != "" {
				*lines = append(*lines, prefix+text)
			}
			if block.Expression != "" {
				*lines = append(*lines, prefix+block.Expression)
			}
			if len(block.Blocks) > 0 {
				appendRichBlocksText(lines, block.Blocks, prefix)
			}
			if caption := richTextJSONValue(block.Caption); caption != "" {
				*lines = append(*lines, prefix+caption)
			}
			if credit := richTextJSONValue(block.Credit); credit != "" {
				*lines = append(*lines, prefix+credit)
			}
		}
	}
}

func richTextJSONValue(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	switch raw[0] {
	case '"':
		var value string
		if json.Unmarshal(raw, &value) == nil {
			return value
		}
	case '[':
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) == nil {
			var b strings.Builder
			for _, item := range items {
				b.WriteString(richTextJSONValue(item))
			}
			return b.String()
		}
	case '{':
		var value struct {
			Text            json.RawMessage `json:"text"`
			AlternativeText string          `json:"alternative_text"`
			Expression      string          `json:"expression"`
		}
		if json.Unmarshal(raw, &value) != nil {
			return ""
		}
		if len(value.Text) > 0 {
			return richTextJSONValue(value.Text)
		}
		if value.AlternativeText != "" {
			return value.AlternativeText
		}
		return value.Expression
	}
	return ""
}

func stripHTML(value string) string {
	var b strings.Builder
	inTag := false
	for _, r := range value {
		switch r {
		case '<':
			inTag = true
		case '>':
			inTag = false
		default:
			if !inTag {
				b.WriteRune(r)
			}
		}
	}
	return html.UnescapeString(b.String())
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

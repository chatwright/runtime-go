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

// outgoing is a single Bot API call the bot made to the emulator.
type outgoing struct {
	chatID     int64
	text       string
	markup     *tgbotapi.InlineKeyboardMarkup
	receivedAt time.Time
}

// Emulator is an in-process HTTP server emulating the Telegram Bot API.
type Emulator struct {
	server *httptest.Server

	mu            sync.Mutex
	calls         []*outgoing
	nextMessageID int
	updated       chan struct{} // closed (and replaced) on every new call; broadcast signal
}

// NewEmulator starts a fake Telegram Bot API server on a random local port.
func NewEmulator() *Emulator {
	e := &Emulator{
		nextMessageID: 1,
		updated:       make(chan struct{}),
	}
	e.server = httptest.NewServer(http.HandlerFunc(e.handle))
	return e
}

// BotAPIURL is the base URL the bot-under-test should use as its Telegram Bot API
// host, in place of https://api.telegram.org.
func (e *Emulator) BotAPIURL() string { return e.server.URL }

// Close shuts down the emulator's HTTP server.
func (e *Emulator) Close() { e.server.Close() }

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

// handle routes /bot<token>/<method> like the real Bot API.
func (e *Emulator) handle(w http.ResponseWriter, r *http.Request) {
	_, method := parsePath(r.URL.Path)

	switch method {
	case "getMe":
		writeResult(w, tgbotapi.User{ID: 1, IsBot: true, FirstName: "ChatwrightBot", UserName: "chatwright_bot"})
	case "sendMessage":
		e.handleSendMessage(w, r)
	default:
		// Be lenient: acknowledge any other method (setWebhook, deleteWebhook,
		// answerCallbackQuery, setMyCommands, ...) so arbitrary bots don't error.
		writeResult(w, true)
	}
}

func (e *Emulator) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	chatID, text, markup := parseSendMessage(r)

	e.mu.Lock()
	o := &outgoing{chatID: chatID, text: text, markup: markup, receivedAt: time.Now()}
	e.calls = append(e.calls, o)
	messageID := e.nextMessageID
	e.nextMessageID++
	close(e.updated)
	e.updated = make(chan struct{})
	e.mu.Unlock()

	writeResult(w, tgbotapi.Message{
		MessageID: messageID,
		From:      &tgbotapi.User{ID: 1, IsBot: true, FirstName: "ChatwrightBot"},
		Chat:      &tgbotapi.Chat{ID: chatID, Type: "private"},
		Date:      int(o.receivedAt.Unix()),
		Text:      text,
	})
}

// WaitForMessage waits for the (consumed+1)-th outbound message to chatID and
// returns it as a neutral platform.Message.
func (e *Emulator) WaitForMessage(chatID int64, consumed int, timeout time.Duration) (*platform.Message, bool) {
	deadline := time.After(timeout)
	for {
		e.mu.Lock()
		var match *outgoing
		seen := 0
		for _, c := range e.calls {
			if c.chatID == chatID {
				if seen == consumed {
					match = c
					break
				}
				seen++
			}
		}
		ch := e.updated
		e.mu.Unlock()

		if match != nil {
			return normalize(match), true
		}
		select {
		case <-ch:
		case <-deadline:
			return nil, false
		}
	}
}

// normalize converts a captured Telegram call into a neutral message.
func normalize(o *outgoing) *platform.Message {
	m := &platform.Message{
		Platform:   "telegram",
		ChatID:     o.chatID,
		Text:       o.text,
		ReceivedAt: o.receivedAt,
	}
	if o.markup != nil {
		for _, row := range o.markup.InlineKeyboard {
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

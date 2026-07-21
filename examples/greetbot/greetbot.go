// Package greetbot is a minimal, real Telegram bot used to exercise Chatwright
// end-to-end. It speaks the Telegram Bot API via the tgbotapi client library; its
// outbound calls are redirected to whatever Bot API host it is constructed with
// (Chatwright's emulator in tests, https://api.telegram.org in production).
//
// It is intentionally tiny: /start offers a language choice — picking one edits
// that same message in place, translating it, and is remembered for the rest of
// the chat — and /time replies with the current time. The point is to prove a
// genuine Telegram-protocol bot — parsing real updates, tracking per-chat state,
// editing its own messages, and sending via the real client — can be driven by
// Chatwright over HTTP.
package greetbot

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bots-go-framework/bots-api-telegram/tgbotapi"
)

// defaultLanguage is used until a chat picks one.
const defaultLanguage = "en"

// callbackLangPrefix marks a language-selection callback, e.g. "lang:es".
const callbackLangPrefix = "lang:"

// languages maps a language code to its display name (button label) and its
// greeting reply text.
var languages = []struct {
	code, label, greeting string
}{
	{"en", "English", "Howdy stranger"},
	{"es", "Español", "¡Hola, forastero!"},
	{"fr", "Français", "Salut l'inconnu"},
}

func greetingFor(code string) string {
	for _, l := range languages {
		if l.code == code {
			return l.greeting
		}
	}
	return greetingFor(defaultLanguage)
}

// TimeLayout is the format /time replies use. Exported so tests can compute the
// exact expected reply for a given instant without duplicating the format.
const TimeLayout = "15:04:05 MST"

// FormatTime renders t (in UTC) the way /time replies do.
func FormatTime(t time.Time) string {
	return t.UTC().Format(TimeLayout)
}

// Bot is a minimal Telegram bot with per-chat language state.
type Bot struct {
	api *tgbotapi.BotAPI
	now func() time.Time

	mu   sync.Mutex
	lang map[int64]string // chatID -> selected language code
}

// Option configures a Bot at construction time.
type Option func(*Bot)

// WithClock overrides the bot's notion of "now", used by /time. Tests use it to
// get a deterministic, assertable reply instead of racing the wall clock.
func WithClock(now func() time.Time) Option {
	return func(b *Bot) { b.now = now }
}

// New builds a bot whose Telegram Bot API calls go to apiBaseURL (e.g.
// Chatwright's emulator URL). token is the bot token used in the API path.
func New(apiBaseURL, token string, opts ...Option) *Bot {
	client := &http.Client{Transport: redirect(apiBaseURL)}
	b := &Bot{
		api:  tgbotapi.NewBotAPIWithClient(token, client),
		now:  time.Now,
		lang: make(map[int64]string),
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Handler returns the bot's webhook handler. Point Chatwright at it with
// ServeWebhook, or run it on a real listener and use WebhookAt.
func (b *Bot) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var update tgbotapi.Update
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch {
		case update.CallbackQuery != nil:
			b.handleCallback(update.CallbackQuery)
		case update.Message != nil:
			b.handleMessage(update.Message)
		}
		w.WriteHeader(http.StatusOK)
	})
}

func (b *Bot) handleMessage(msg *tgbotapi.Message) {
	if msg.Chat == nil {
		return
	}
	if msg.Text == "/start" {
		reply := tgbotapi.NewMessage(msg.Chat.ID, "Choose your language")
		rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(languages))
		for _, l := range languages {
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(l.label, callbackLangPrefix+l.code),
			))
		}
		reply.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
		_, _ = b.api.Send(reply)
		return
	}

	if msg.Text == "/time" {
		reply := tgbotapi.NewMessage(msg.Chat.ID, FormatTime(b.now()))
		_, _ = b.api.Send(reply)
		return
	}

	reply := tgbotapi.NewMessage(msg.Chat.ID, greetingFor(b.langFor(msg.Chat.ID)))
	_, _ = b.api.Send(reply)
}

func (b *Bot) handleCallback(cb *tgbotapi.CallbackQuery) {
	if cb.Message == nil || cb.Message.Chat == nil {
		return
	}
	code, ok := strings.CutPrefix(cb.Data, callbackLangPrefix)
	if !ok {
		return
	}
	b.setLang(cb.Message.Chat.ID, code)
	// Edit the language-choice message in place, translating it to the
	// selected language, rather than sending a new message.
	edit := tgbotapi.NewEditMessageText(cb.Message.Chat.ID, cb.Message.MessageID, "", greetingFor(code))
	_, _ = b.api.Send(edit)
}

func (b *Bot) langFor(chatID int64) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if code, ok := b.lang[chatID]; ok {
		return code
	}
	return defaultLanguage
}

func (b *Bot) setLang(chatID int64, code string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lang[chatID] = code
}

// redirect rewrites every request to baseURL's scheme+host, keeping the Telegram
// path (/bot<token>/<method>). This lets the real tgbotapi client — whose
// endpoint is a compile-time constant — target the emulator instead.
func redirect(baseURL string) http.RoundTripper {
	base, _ := url.Parse(baseURL)
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		req.URL.Scheme = base.Scheme
		req.URL.Host = base.Host
		req.Host = base.Host
		return http.DefaultTransport.RoundTrip(req)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

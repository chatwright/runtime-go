// Package greetbot is a minimal, real Telegram bot used to exercise Chatwright
// end-to-end. It speaks the Telegram Bot API via the tgbotapi client library; its
// outbound calls are redirected to whatever Bot API host it is constructed with
// (Chatwright's emulator in tests, https://api.telegram.org in production).
//
// It is intentionally tiny: greet on any message, and offer an inline button on
// /start. The point is to prove a genuine Telegram-protocol bot — parsing real
// updates and sending via the real client — can be driven by Chatwright over HTTP.
package greetbot

import (
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/bots-go-framework/bots-api-telegram/tgbotapi"
)

// Bot is a minimal Telegram bot.
type Bot struct {
	api *tgbotapi.BotAPI
}

// New builds a bot whose Telegram Bot API calls go to apiBaseURL (e.g.
// Chatwright's emulator URL). token is the bot token used in the API path.
func New(apiBaseURL, token string) *Bot {
	client := &http.Client{Transport: redirect(apiBaseURL)}
	return &Bot{api: tgbotapi.NewBotAPIWithClient(token, client)}
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
		if msg := update.Message; msg != nil && msg.Chat != nil {
			reply := tgbotapi.NewMessage(msg.Chat.ID, "Howdy stranger")
			if msg.Text == "/start" {
				reply.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
					tgbotapi.NewInlineKeyboardRow(
						tgbotapi.NewInlineKeyboardButtonData("My events", "my-events"),
					),
				)
			}
			_, _ = b.api.Send(reply)
		}
		w.WriteHeader(http.StatusOK)
	})
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

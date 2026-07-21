package chatwright_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chatwright/chatwright"
	"github.com/chatwright/chatwright/whatsapp"
)

// The greeter bots below are deliberately framework-agnostic: plain net/http, no
// bots-go-framework, no SDK. They prove Chatwright can drive a bot written in
// anything, purely over HTTP.

// tgGreeter is a Telegram bot: it receives an update on its webhook and calls the
// Telegram Bot API (Chatwright's emulator) to reply. "/start" adds an inline
// button; anything else gets a plain greeting.
func tgGreeter(botAPIURL, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var upd struct {
			Message struct {
				Chat struct {
					ID int64 `json:"id"`
				} `json:"chat"`
				Text string `json:"text"`
			} `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&upd); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		reply := map[string]any{"chat_id": upd.Message.Chat.ID, "text": "Howdy stranger"}
		if upd.Message.Text == "/start" {
			reply["reply_markup"] = map[string]any{
				"inline_keyboard": [][]map[string]any{{
					{"text": "My events", "callback_data": "my-events"},
				}},
			}
		}
		post(botAPIURL+"/bot"+token+"/sendMessage", reply)
		w.WriteHeader(http.StatusOK)
	}
}

// waGreeter is a WhatsApp Cloud API bot: it receives an inbound webhook and calls
// the Graph API (Chatwright's emulator) to reply with text.
func waGreeter(botAPIURL, phoneNumberID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Entry []struct {
				Changes []struct {
					Value struct {
						Messages []struct {
							From string `json:"from"`
						} `json:"messages"`
					} `json:"value"`
				} `json:"changes"`
			} `json:"entry"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var from string
		if len(in.Entry) > 0 && len(in.Entry[0].Changes) > 0 && len(in.Entry[0].Changes[0].Value.Messages) > 0 {
			from = in.Entry[0].Changes[0].Value.Messages[0].From
		}
		post(strings.TrimSuffix(botAPIURL, "/")+"/v20.0/"+phoneNumberID+"/messages", map[string]any{
			"messaging_product": "whatsapp",
			"to":                from,
			"type":              "text",
			"text":              map[string]string{"body": "Howdy stranger"},
		})
		w.WriteHeader(http.StatusOK)
	}
}

func post(url string, payload any) {
	body, _ := json.Marshal(payload)
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

// greetScenario is a single platform-agnostic scenario. It uses only neutral
// verbs, so the same function runs unchanged on any platform.
func greetScenario(cw *chatwright.Chatwright) {
	chat := cw.PrivateChat(chatwright.User{ID: "alice", FirstName: "Alice"})
	chat.SendText("Hi")
	chat.ExpectBotMessage().
		Within(time.Second).
		Text("Howdy stranger")
}

// TestGreeting_CrossPlatform runs the one neutral scenario against both Telegram
// and WhatsApp, proving scenarios are platform-agnostic and Chatwright maps them
// to platform-specific calls.
func TestGreeting_CrossPlatform(t *testing.T) {
	t.Run("telegram", func(t *testing.T) {
		cw := chatwright.New(t) // Telegram is the default platform
		cw.ServeWebhook(tgGreeter(cw.BotAPIURL(), "TEST:TOKEN"))
		greetScenario(cw)
	})

	t.Run("whatsapp", func(t *testing.T) {
		cw := chatwright.New(t, chatwright.OnPlatform(whatsapp.Platform()))
		cw.ServeWebhook(waGreeter(cw.BotAPIURL(), "chatwright-phone"))
		greetScenario(cw)
	})
}

// TestTelegramActions exercises interactive actions (inline buttons) via the
// neutral ExpectAction API.
func TestTelegramActions(t *testing.T) {
	cw := chatwright.New(t)
	cw.ServeWebhook(tgGreeter(cw.BotAPIURL(), "TEST:TOKEN"))

	chat := cw.PrivateChat(chatwright.User{ID: "alice", FirstName: "Alice"})
	chat.SendText("/start")

	msg := chat.ExpectBotMessage().Within(time.Second).IsTextMessage()
	msg.Text("Howdy stranger")
	msg.ExpectAction(0, 0).
		Label("My events").
		ID("my-events")
}

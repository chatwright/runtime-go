package chatwright_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/chatwright/chatwright"
	"github.com/chatwright/chatwright/whatsapp"
)

// editingGreeter is a minimal Telegram bot used to exercise the emulator's
// shared per-chat message-ID sequence and edit journal: "/start" sends
// "Hello"; "/edit" edits that same message to "Hello, edited". It remembers
// only the most recently sent message ID per chat. The edit call is
// form-encoded (as the real tgbotapi client sends it today; the emulator's
// editMessageText handler does not yet parse JSON bodies — that lands in a
// later step).
func editingGreeter(botAPIURL, token string) http.HandlerFunc {
	var mu sync.Mutex
	lastMsgID := map[int64]int{}

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
		chatID := upd.Message.Chat.ID

		switch upd.Message.Text {
		case "/edit":
			mu.Lock()
			id := lastMsgID[chatID]
			mu.Unlock()
			form := url.Values{
				"chat_id":    {strconv.FormatInt(chatID, 10)},
				"message_id": {strconv.Itoa(id)},
				"text":       {"Hello, edited"},
			}
			resp, err := http.PostForm(botAPIURL+"/bot"+token+"/editMessageText", form)
			if err == nil {
				_ = resp.Body.Close()
			}
		default:
			result := postForResult(botAPIURL+"/bot"+token+"/sendMessage", map[string]any{
				"chat_id": chatID,
				"text":    "Hello",
			})
			if id, ok := result["message_id"].(float64); ok {
				mu.Lock()
				lastMsgID[chatID] = int(id)
				mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusOK)
	}
}

// postForResult POSTs a JSON payload and returns the Bot API envelope's
// decoded "result" object (nil on any error).
func postForResult(url string, payload any) map[string]any {
	body, _ := json.Marshal(payload)
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	var env struct {
		Result map[string]any `json:"result"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	return env.Result
}

// TestSharedPerChatMessageIDSequence proves the fix for the message-ID
// collision bug: inbound and outbound messages in a chat draw from the same
// per-chat counter, so the bot's reply to the chat's first message never
// collides with it.
func TestSharedPerChatMessageIDSequence(t *testing.T) {
	cw := chatwright.New(t)
	cw.ServeWebhook(editingGreeter(cw.BotAPIURL(), "TEST:TOKEN"))

	chat := cw.PrivateChat(chatwright.User{ID: "alice", FirstName: "Alice"})
	chat.SendText("/start") // consumes message ID 1 in this chat

	msg := chat.ExpectBotMessage().Within(time.Second).IsTextMessage()
	msg.Text("Hello")
	if got := msg.Snapshot().MessageID; got != 2 {
		t.Fatalf("bot's reply message ID = %d, want 2 (the chat's first message, \"/start\", took ID 1)", got)
	}

	chat.SendText("/edit") // consumes message ID 3; edits message 2 in place
	msg.ExpectEdited().Within(time.Second).Text("Hello, edited")
}

// TestTranscriptShowsInboundOutboundAndEdits drives a short conversation with
// an edit, then forces a timeout to inspect the transcript embedded in the
// failure message: it must show the inbound text, the edited message's
// current (not original) text annotated as edited, and nothing about the
// exchange lost.
func TestTranscriptShowsInboundOutboundAndEdits(t *testing.T) {
	fake := newFakeTB()
	failed, logs := fake.run(func(tb testing.TB) {
		cw := chatwright.New(tb)
		cw.ServeWebhook(editingGreeter(cw.BotAPIURL(), "TEST:TOKEN"))

		chat := cw.PrivateChat(chatwright.User{ID: "alice", FirstName: "Alice"})
		chat.SendText("/start")
		msg := chat.ExpectBotMessage().Within(time.Second).Text("Hello")

		chat.SendText("/edit")
		msg.ExpectEdited().Within(time.Second).Text("Hello, edited")

		// Nothing more is coming: force a timeout to capture the transcript.
		chat.ExpectBotMessage().Within(50 * time.Millisecond).IsTextMessage()
	})
	if !failed {
		t.Fatalf("expected the final ExpectBotMessage to time out")
	}
	for _, want := range []string{
		"[1 user] /start",
		"[2 bot] Hello, edited (v2, edited)",
		"[3 user] /edit",
	} {
		if !anyContains(logs, want) {
			t.Fatalf("expected the transcript to contain %q, got: %v", want, logs)
		}
	}
}

// TestWhatsAppTranscriptOnTimeout mirrors the Telegram transcript coverage
// for the WhatsApp emulator: it has no edit concept, but inbound/outbound
// text should still show up in a timeout failure's transcript dump.
func TestWhatsAppTranscriptOnTimeout(t *testing.T) {
	fake := newFakeTB()
	failed, logs := fake.run(func(tb testing.TB) {
		cw := chatwright.New(tb, chatwright.OnPlatform(whatsapp.Platform()))
		cw.ServeWebhook(waGreeter(cw.BotAPIURL(), "chatwright-phone"))

		chat := cw.PrivateChat(chatwright.User{ID: "alice", FirstName: "Alice"})
		chat.SendText("Hi")
		chat.ExpectBotMessage().Within(time.Second).Text("Howdy stranger")

		chat.ExpectBotMessage().Within(50 * time.Millisecond).IsTextMessage()
	})
	if !failed {
		t.Fatalf("expected the final ExpectBotMessage to time out")
	}
	for _, want := range []string{"[1 user] Hi", "[2 bot] Howdy stranger"} {
		if !anyContains(logs, want) {
			t.Fatalf("expected the transcript to contain %q, got: %v", want, logs)
		}
	}
}

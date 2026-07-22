package chatwright_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/chatwright/chatwright"
	"github.com/chatwright/chatwright/whatsapp"
)

// runPollingGreeter starts a background goroutine that long-polls getUpdates
// against botAPIURL — never registering a webhook — and replies "Howdy
// stranger" to every text message it sees, exactly like tgGreeter's default
// case but driven by polling instead of a push. It proves the emulator, not
// the harness, now owns delivery: Telegram bots that never register a
// webhook (a large share of real bots) are drivable at all.
func runPollingGreeter(t *testing.T, botAPIURL, token string) {
	t.Helper()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	go func() {
		offset := 0
		client := &http.Client{Timeout: 3 * time.Second}
		for {
			select {
			case <-stop:
				return
			default:
			}

			url := fmt.Sprintf("%s/bot%s/getUpdates?offset=%d&timeout=1", botAPIURL, token, offset)
			resp, err := client.Get(url)
			if err != nil {
				continue
			}
			var env struct {
				Result []struct {
					UpdateID int `json:"update_id"`
					Message  *struct {
						Chat struct {
							ID int64 `json:"id"`
						} `json:"chat"`
						Text string `json:"text"`
					} `json:"message"`
				} `json:"result"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&env)
			_ = resp.Body.Close()

			for _, u := range env.Result {
				if u.UpdateID >= offset {
					offset = u.UpdateID + 1
				}
				if u.Message != nil {
					post(botAPIURL+"/bot"+token+"/sendMessage", map[string]any{
						"chat_id": u.Message.Chat.ID,
						"text":    "Howdy stranger",
					})
				}
			}
		}
	}()
}

// TestTelegramPollingBot drives a bot that only ever calls getUpdates — it
// never registers a webhook — proving SubmitText queues updates for
// getUpdates when no webhook is configured, and that the offset/timeout
// long-poll subset is good enough to actually deliver and drain them. The
// public test API is unchanged: SendText/ExpectBotMessage don't know or care
// which delivery strategy is in play.
func TestTelegramPollingBot(t *testing.T) {
	cw := chatwright.New(t) // no ServeWebhook/WebhookAt: this bot polls instead
	runPollingGreeter(t, cw.BotAPIURL(), "TEST:TOKEN")

	chat := cw.PrivateChat(chatwright.User{ID: "alice", FirstName: "Alice"})
	chat.SendText("Hi")
	chat.ExpectBotMessage().Within(3 * time.Second).Text("Howdy stranger")

	// A second round trip proves the offset correctly acknowledges the first
	// batch: the bot doesn't get handed "Hi" again, only the new message.
	chat.SendText("Hi again")
	chat.ExpectBotMessage().Within(3 * time.Second).Text("Howdy stranger")
}

// TestWhatsApp_SubmitWithoutWebhook_Fails proves the WhatsApp emulator, which
// has no polling mode to fall back to, fails Submit* calls with a clear
// message when no webhook is configured — rather than silently queuing
// updates nothing will ever retrieve.
func TestWhatsApp_SubmitWithoutWebhook_Fails(t *testing.T) {
	fake := newFakeTB()
	failed, logs := fake.run(func(tb testing.TB) {
		cw := chatwright.New(tb, chatwright.OnPlatform(whatsapp.Platform()))
		// Deliberately no ServeWebhook/WebhookAt call.
		chat := cw.PrivateChat(chatwright.User{ID: "alice", FirstName: "Alice"})
		chat.SendText("Hi")
	})
	if !failed {
		t.Fatalf("expected SendText to fail without a configured webhook")
	}
	if !anyContains(logs, "no webhook configured") {
		t.Fatalf("expected a no-webhook-configured error, got: %v", logs)
	}
}

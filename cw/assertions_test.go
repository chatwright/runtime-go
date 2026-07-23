package cw_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"chatwright.dev/runtime/cw"
)

// silentGreeter behaves like tgGreeter's default case, except it never replies
// to a message whose text is exactly "/silent" — used to exercise
// ExpectNoMessage's true-negative (no reply arrives) path.
func silentGreeter(botAPIURL, token string) http.HandlerFunc {
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
		if upd.Message.Text != "/silent" {
			post(botAPIURL+"/bot"+token+"/sendMessage", map[string]any{
				"chat_id": upd.Message.Chat.ID,
				"text":    "Howdy stranger",
			})
		}
		w.WriteHeader(http.StatusOK)
	}
}

func TestTextContainsAndMatches(t *testing.T) {
	w := cw.New(t)
	w.ServeWebhook(tgGreeter(w.BotAPIURL(), "TEST:TOKEN"))

	chat := w.PrivateChat(cw.User{ID: "alice", FirstName: "Alice"})
	chat.SendText("Hi")
	chat.ExpectBotMessage().
		Within(time.Second).
		TextContains("stranger").
		TextMatches(`^Howdy\s`)
}

func TestTextMatches_InvalidPattern_FailsImmediately(t *testing.T) {
	fake := newFakeTB()
	failed, logs := fake.run(func(tb testing.TB) {
		w := cw.New(tb)
		w.ServeWebhook(tgGreeter(w.BotAPIURL(), "TEST:TOKEN"))
		chat := w.PrivateChat(cw.User{ID: "alice", FirstName: "Alice"})
		chat.SendText("Hi")
		chat.ExpectBotMessage().Within(time.Second).TextMatches("[")
	})
	if !failed {
		t.Fatalf("expected the scenario to fail on an invalid regexp pattern")
	}
	if !anyContains(logs, "invalid pattern") {
		t.Fatalf("expected a failure message about the invalid pattern, got: %v", logs)
	}
}

func TestExpectNoMessage_NoReply_Passes(t *testing.T) {
	w := cw.New(t)
	w.ServeWebhook(silentGreeter(w.BotAPIURL(), "TEST:TOKEN"))
	chat := w.PrivateChat(cw.User{ID: "alice", FirstName: "Alice"})
	chat.SendText("/silent")
	chat.ExpectNoMessage(100 * time.Millisecond)
}

func TestExpectNoMessage_UnexpectedReply_Fails(t *testing.T) {
	fake := newFakeTB()
	failed, logs := fake.run(func(tb testing.TB) {
		w := cw.New(tb)
		w.ServeWebhook(tgGreeter(w.BotAPIURL(), "TEST:TOKEN"))
		chat := w.PrivateChat(cw.User{ID: "alice", FirstName: "Alice"})
		chat.SendText("Hi")
		chat.ExpectNoMessage(time.Second)
	})
	if !failed {
		t.Fatalf("expected ExpectNoMessage to fail: the bot did reply within the window")
	}
	if !anyContains(logs, "Howdy stranger") {
		t.Fatalf("expected the failure message to include the unexpected reply text, got: %v", logs)
	}
}

func TestPrivateChat_HandleAliasing(t *testing.T) {
	w := cw.New(t)
	w.ServeWebhook(tgGreeter(w.BotAPIURL(), "TEST:TOKEN"))

	user := cw.User{ID: "alice", FirstName: "Alice"}
	chat1 := w.PrivateChat(user)
	chat2 := w.PrivateChat(user)
	if chat1 != chat2 {
		t.Fatalf("PrivateChat returned different handles for the same user; want the same cached *Chat")
	}

	chat1.SendText("Hi")
	// The consumption cursor is shared: asserting through the second handle
	// must see the one reply already sent — not wait for a second one that
	// never comes (which double-consumption would require).
	chat2.ExpectBotMessage().Within(time.Second).Text("Howdy stranger")
}

func TestBotMessage_NeverResolved_FailsAtCleanup(t *testing.T) {
	fake := newFakeTB()
	failed, logs := fake.run(func(tb testing.TB) {
		w := cw.New(tb)
		w.ServeWebhook(tgGreeter(w.BotAPIURL(), "TEST:TOKEN"))
		chat := w.PrivateChat(cw.User{ID: "alice", FirstName: "Alice"})
		chat.SendText("Hi")
		chat.ExpectBotMessage() // never asserted on — must fail at cleanup
	})
	if !failed {
		t.Fatalf("expected an unresolved BotMessage expectation to fail the test at cleanup")
	}
	if !anyContains(logs, "never resolved") {
		t.Fatalf("expected the cleanup failure to explain the message was never resolved, got: %v", logs)
	}
}

func TestWithin_AfterResolved_FailsImmediately(t *testing.T) {
	fake := newFakeTB()
	failed, logs := fake.run(func(tb testing.TB) {
		w := cw.New(tb)
		w.ServeWebhook(tgGreeter(w.BotAPIURL(), "TEST:TOKEN"))
		chat := w.PrivateChat(cw.User{ID: "alice", FirstName: "Alice"})
		chat.SendText("Hi")
		msg := chat.ExpectBotMessage().Within(time.Second)
		msg.Text("Howdy stranger")
		msg.Within(2 * time.Second) // called after resolution — must fail immediately
	})
	if !failed {
		t.Fatalf("expected calling Within after resolution to fail the test immediately")
	}
	if !anyContains(logs, "already resolved") {
		t.Fatalf("expected the failure message to explain the message was already resolved, got: %v", logs)
	}
}

// anyContains reports whether any log line contains substr.
func anyContains(logs []string, substr string) bool {
	for _, l := range logs {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

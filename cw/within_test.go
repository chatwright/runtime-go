package cw_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"chatwright.dev/runtime/cw"
)

// delayedGreeter replies "Howdy stranger" after the given delay, so tests can
// exercise latency-budget diagnostics deterministically. The delay happens
// inside the webhook handler itself, so SendText (which blocks until the
// handler returns) also observes it.
func delayedGreeter(botAPIURL, token string, delay time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var upd struct {
			Message struct {
				Chat struct {
					ID int64 `json:"id"`
				} `json:"chat"`
			} `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&upd); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		time.Sleep(delay)
		post(botAPIURL+"/bot"+token+"/sendMessage", map[string]any{
			"chat_id": upd.Message.Chat.ID,
			"text":    "Howdy stranger",
		})
		w.WriteHeader(http.StatusOK)
	}
}

// TestWithin_LateReply_FailsWithLatencyDiagnostic proves the AC the old
// conflated Within could never satisfy: a reply that arrives after its
// budget but well before the safety timeout must still be observed, and the
// failure must show the observed latency, the budget, and the reply's actual
// text — not a bare "none arrived" timeout.
func TestWithin_LateReply_FailsWithLatencyDiagnostic(t *testing.T) {
	fake := newFakeTB()
	failed, logs := fake.run(func(tb testing.TB) {
		w := cw.New(tb) // default 5s safety timeout comfortably covers the 150ms delay below
		w.ServeWebhook(delayedGreeter(w.BotAPIURL(), "TEST:TOKEN", 150*time.Millisecond))

		chat := w.PrivateChat(cw.User{ID: "alice", FirstName: "Alice"})
		chat.SendText("Hi")
		chat.ExpectBotMessage().Within(50 * time.Millisecond).Text("Howdy stranger")
	})
	if !failed {
		t.Fatalf("expected the reply to fail its 50ms latency budget")
	}
	if !anyContains(logs, "budget 50ms") {
		t.Fatalf("expected the failure to name the 50ms budget, got: %v", logs)
	}
	if !anyContains(logs, `"Howdy stranger"`) {
		t.Fatalf("expected the failure to show the late reply's actual text, got: %v", logs)
	}
	// The reply's content was correct, just late: Text() itself must not
	// also report a content mismatch — the diagnostic is about latency.
	if anyContains(logs, "bot message text") {
		t.Fatalf("Text() should not report a content mismatch for a late-but-correct reply, got: %v", logs)
	}
}

// TestWithin_BudgetLargerThanSafetyTimeout_ExtendsWait proves Within can
// never be undercut by a smaller configured safety timeout: the observation
// window extends to cover a generous per-assertion budget.
func TestWithin_BudgetLargerThanSafetyTimeout_ExtendsWait(t *testing.T) {
	w := cw.New(t, cw.WithSafetyTimeout(50*time.Millisecond))
	w.ServeWebhook(delayedGreeter(w.BotAPIURL(), "TEST:TOKEN", 150*time.Millisecond))

	chat := w.PrivateChat(cw.User{ID: "alice", FirstName: "Alice"})
	chat.SendText("Hi")
	// The budget (300ms) exceeds the configured safety timeout (50ms): the
	// wait must extend to the budget rather than truncate to the timeout, so
	// this passes even though the reply arrives well after 50ms.
	chat.ExpectBotMessage().Within(300 * time.Millisecond).Text("Howdy stranger")
}

// TestWithin_NoReply_FailsAtSafetyTimeoutWithTranscript exercises the "no
// reply at all" path: with no Within budget set, the wait runs to the
// configured safety timeout, and the failure names it as such and includes
// the transcript (see TestTranscriptShowsInboundOutboundAndEdits for fuller
// transcript-content coverage).
func TestWithin_NoReply_FailsAtSafetyTimeoutWithTranscript(t *testing.T) {
	fake := newFakeTB()
	failed, logs := fake.run(func(tb testing.TB) {
		w := cw.New(tb, cw.WithSafetyTimeout(50*time.Millisecond))
		w.ServeWebhook(silentGreeter(w.BotAPIURL(), "TEST:TOKEN"))

		chat := w.PrivateChat(cw.User{ID: "alice", FirstName: "Alice"})
		chat.SendText("/silent")
		chat.ExpectBotMessage().IsTextMessage()
	})
	if !failed {
		t.Fatalf("expected no reply within the safety timeout to fail the test")
	}
	if !anyContains(logs, "50ms (safety timeout)") {
		t.Fatalf("expected the failure to name the safety timeout explicitly, got: %v", logs)
	}
	if !anyContains(logs, "chat ") || !anyContains(logs, "transcript") {
		t.Fatalf("expected the failure to include the transcript dump, got: %v", logs)
	}
}

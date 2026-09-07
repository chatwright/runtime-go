package claudechannel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"chatwright.dev/runtime/platform"
)

func TestEmulator_ImplementsPlatformEmulator(t *testing.T) {
	var _ platform.Emulator = NewEmulator()
}

func TestEmulator_SubmitText_QueuesInbox(t *testing.T) {
	e := NewEmulator()
	defer e.Close()

	if err := e.SubmitText(1, platform.User{ID: 1, FirstName: "Alice"}, "hi"); err != nil {
		t.Fatalf("SubmitText: %v", err)
	}

	resp, err := http.Get(e.BotAPIURL() + "/inbox?wait=1")
	if err != nil {
		t.Fatalf("GET /inbox: %v", err)
	}
	defer resp.Body.Close()
	var msgs []inboxMessage
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Text != "hi" || msgs[0].ChatID != 1 || msgs[0].User != "Alice" {
		t.Fatalf("unexpected inbox contents: %+v", msgs)
	}
}

func TestEmulator_Inbox_TimesOutEmpty(t *testing.T) {
	e := NewEmulator()
	defer e.Close()

	start := time.Now()
	resp, err := http.Get(e.BotAPIURL() + "/inbox?wait=0.2")
	if err != nil {
		t.Fatalf("GET /inbox: %v", err)
	}
	defer resp.Body.Close()
	var msgs []inboxMessage
	_ = json.NewDecoder(resp.Body).Decode(&msgs)
	if len(msgs) != 0 {
		t.Fatalf("expected empty timeout result, got %+v", msgs)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("returned too fast: %v", elapsed)
	}
}

func TestEmulator_Reply_FulfillsWaitForMessage(t *testing.T) {
	e := NewEmulator()
	defer e.Close()

	done := make(chan *platform.Message, 1)
	go func() {
		msg, ok := e.WaitForMessage(42, 0, 2*time.Second)
		if !ok {
			done <- nil
			return
		}
		done <- msg
	}()

	time.Sleep(50 * time.Millisecond) // let WaitForMessage start listening
	postJSON(t, e.BotAPIURL()+"/reply", replyRequest{ChatID: 42, Text: "hello"})

	msg := <-done
	if msg == nil {
		t.Fatal("WaitForMessage timed out")
	}
	if msg.Text != "hello" || msg.ChatID != 42 || msg.Version != 0 {
		t.Fatalf("unexpected message: %+v", msg)
	}
}

func TestEmulator_Edit_FulfillsWaitForEdit(t *testing.T) {
	e := NewEmulator()
	defer e.Close()

	var reply replyResponse
	postJSONInto(t, e.BotAPIURL()+"/reply", replyRequest{ChatID: 7, Text: "v0"}, &reply)

	done := make(chan *platform.Message, 1)
	go func() {
		msg, ok := e.WaitForEdit(7, reply.MessageID, 0, 2*time.Second)
		if !ok {
			done <- nil
			return
		}
		done <- msg
	}()

	time.Sleep(50 * time.Millisecond)
	postJSON(t, e.BotAPIURL()+"/edit", editRequest{ChatID: 7, MessageID: reply.MessageID, Text: "v1"})

	msg := <-done
	if msg == nil {
		t.Fatal("WaitForEdit timed out")
	}
	if msg.Text != "v1" || msg.Version != 1 {
		t.Fatalf("unexpected edited message: %+v", msg)
	}

	// WaitForMessage should still see the message's latest version at slot 0.
	latest, ok := e.WaitForMessage(7, 0, time.Second)
	if !ok || latest.Text != "v1" {
		t.Fatalf("WaitForMessage did not see latest edit: %+v ok=%v", latest, ok)
	}
}

func TestEmulator_SubmitClick_NotSupported(t *testing.T) {
	e := NewEmulator()
	defer e.Close()

	err := e.SubmitClick(1, platform.User{ID: 1}, "data", 1)
	if err == nil {
		t.Fatal("expected an error: claudechannel has no interactive actions")
	}
}

func TestEmulator_Journal_And_Transcript(t *testing.T) {
	e := NewEmulator()
	defer e.Close()

	_ = e.SubmitText(9, platform.User{ID: 1, FirstName: "Bob"}, "ping")
	postJSON(t, e.BotAPIURL()+"/reply", replyRequest{ChatID: 9, Text: "pong"})
	postJSON(t, e.BotAPIURL()+"/journal", journalRequest{ChatID: 9, Method: "some_tool", Text: "sighted"})

	entries, err := e.Journal(9)
	if err != nil {
		t.Fatalf("Journal: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 journal entries, got %d: %+v", len(entries), entries)
	}
	if entries[0].Direction != platform.DirectionUser || entries[0].Text != "ping" {
		t.Fatalf("entry 0: %+v", entries[0])
	}
	if entries[1].Direction != platform.DirectionBot || entries[1].Text != "pong" {
		t.Fatalf("entry 1: %+v", entries[1])
	}
	if entries[2].Kind != platform.JournalEntryUncaptured || entries[2].Method != "some_tool" {
		t.Fatalf("entry 2: %+v", entries[2])
	}

	transcript := e.Transcript(9)
	if transcript == "" {
		t.Fatal("expected non-empty transcript")
	}
}

func postJSON(t *testing.T, url string, body any) {
	t.Helper()
	postJSONInto(t, url, body, nil)
}

func postJSONInto(t *testing.T, url string, body, out any) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("POST %s: status %d", url, resp.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
}

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRelay is a minimal in-process stand-in for a
// chatwright.dev/runtime/claudechannel Emulator: enough of GET /inbox,
// POST /reply and POST /edit to exercise the relay binary's JSON-RPC
// handling without building the real emulator.
type fakeRelay struct {
	mu      sync.Mutex
	inbox   []inboxMessage
	wake    chan struct{}
	replies []replyRecorded
	edits   []editRecorded
	nextID  int
}

type replyRecorded struct {
	ChatID  int64
	ReplyTo string
	Text    string
}

type editRecorded struct {
	MessageID int64
	Text      string
}

func newFakeRelay() *fakeRelay {
	return &fakeRelay{wake: make(chan struct{})}
}

func (f *fakeRelay) push(chatID int64, text string) {
	f.mu.Lock()
	f.nextID++
	f.inbox = append(f.inbox, inboxMessage{ID: f.nextID, ChatID: chatID, User: "alice", Text: text, Ts: time.Now()})
	close(f.wake)
	f.wake = make(chan struct{})
	f.mu.Unlock()
}

func (f *fakeRelay) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /inbox", func(w http.ResponseWriter, r *http.Request) {
		deadline := time.After(time.Second)
		for {
			f.mu.Lock()
			if len(f.inbox) > 0 {
				msg := f.inbox[0]
				f.inbox = f.inbox[1:]
				f.mu.Unlock()
				writeJSON(w, []inboxMessage{msg})
				return
			}
			wake := f.wake
			f.mu.Unlock()
			select {
			case <-wake:
				continue
			case <-deadline:
				writeJSON(w, []inboxMessage{})
				return
			}
		}
	})
	mux.HandleFunc("POST /reply", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ChatID  int64  `json:"chatID"`
			ReplyTo string `json:"replyTo"`
			Text    string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.replies = append(f.replies, replyRecorded{ChatID: req.ChatID, ReplyTo: req.ReplyTo, Text: req.Text})
		f.mu.Unlock()
		writeJSON(w, map[string]any{"messageID": 1})
	})
	mux.HandleFunc("POST /edit", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			MessageID int64  `json:"messageID"`
			Text      string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.edits = append(f.edits, editRecorded{MessageID: req.MessageID, Text: req.Text})
		f.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true})
	})
	return httptest.NewServer(mux)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// lineWriter collects newline-delimited JSON-RPC messages written by the
// server under test, one decoded message per call to next().
type lineWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	cond *sync.Cond
}

func newLineWriter() *lineWriter {
	lw := &lineWriter{}
	lw.cond = sync.NewCond(&lw.mu)
	return lw
}

func (lw *lineWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	n, err := lw.buf.Write(p)
	lw.cond.Broadcast()
	return n, err
}

// next blocks until a full line is available and returns it decoded.
func (lw *lineWriter) next(t *testing.T) map[string]any {
	t.Helper()
	lw.mu.Lock()
	defer lw.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if line, ok := lw.tryReadLineLocked(); ok {
			var v map[string]any
			if err := json.Unmarshal(line, &v); err != nil {
				t.Fatalf("decode line %q: %v", line, err)
			}
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for a line; buffered so far: %q", lw.buf.String())
		}
		lw.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		lw.mu.Lock()
	}
}

func (lw *lineWriter) tryReadLineLocked() ([]byte, bool) {
	b := lw.buf.Bytes()
	idx := bytes.IndexByte(b, '\n')
	if idx < 0 {
		return nil, false
	}
	line := make([]byte, idx)
	copy(line, b[:idx])
	lw.buf.Next(idx + 1)
	return line, true
}

func TestServer_InitializeAndToolsList(t *testing.T) {
	relay := newFakeRelay()
	rs := relay.server()
	defer rs.Close()

	s := newServer(rs.URL, "", log.New(io.Discard, "", 0))
	out := newLineWriter()
	in, inWriter := io.Pipe()
	go func() { _ = s.run(in, out) }()
	defer func() { _ = inWriter.Close() }()

	send(t, inWriter, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	resp := out.next(t)
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result, got %+v", resp)
	}
	caps, _ := result["capabilities"].(map[string]any)
	if caps == nil {
		t.Fatalf("expected capabilities, got %+v", result)
	}
	if _, ok := caps["experimental"].(map[string]any)["claude/channel"]; !ok {
		t.Fatalf("expected experimental.claude/channel capability, got %+v", caps)
	}

	send(t, inWriter, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	send(t, inWriter, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	resp = out.next(t)
	result = resp["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %+v", tools)
	}
}

func TestServer_ForwardsInboxAsNotification(t *testing.T) {
	relay := newFakeRelay()
	rs := relay.server()
	defer rs.Close()

	s := newServer(rs.URL, "", log.New(io.Discard, "", 0))
	out := newLineWriter()
	in, inWriter := io.Pipe()
	go func() { _ = s.run(in, out) }()
	defer func() { _ = inWriter.Close() }()

	send(t, inWriter, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	out.next(t) // initialize response
	send(t, inWriter, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	relay.push(99, "hello from user")

	notif := out.next(t)
	if notif["method"] != "notifications/claude/channel" {
		t.Fatalf("expected a claude/channel notification, got %+v", notif)
	}
	params, _ := notif["params"].(map[string]any)
	if params["content"] != "hello from user" {
		t.Fatalf("unexpected content: %+v", params)
	}
	meta, _ := params["meta"].(map[string]any)
	if meta["chat_id"] != "99" {
		t.Fatalf("unexpected chat_id: %+v", meta)
	}
}

func TestServer_ReplyToolCallsRelay(t *testing.T) {
	relay := newFakeRelay()
	rs := relay.server()
	defer rs.Close()

	s := newServer(rs.URL, "", log.New(io.Discard, "", 0))
	out := newLineWriter()
	in, inWriter := io.Pipe()
	go func() { _ = s.run(in, out) }()
	defer func() { _ = inWriter.Close() }()

	send(t, inWriter, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	out.next(t)
	send(t, inWriter, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	send(t, inWriter, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"reply","arguments":{"chat_id":"99","text":"hi back"}}}`)
	resp := out.next(t)
	if _, isErr := resp["error"]; isErr {
		t.Fatalf("unexpected JSON-RPC error: %+v", resp)
	}

	relay.mu.Lock()
	defer relay.mu.Unlock()
	if len(relay.replies) != 1 || relay.replies[0].ChatID != 99 || relay.replies[0].Text != "hi back" {
		t.Fatalf("unexpected replies recorded: %+v", relay.replies)
	}
}

func TestServer_EditMessageToolCallsRelay(t *testing.T) {
	relay := newFakeRelay()
	rs := relay.server()
	defer rs.Close()

	s := newServer(rs.URL, "", log.New(io.Discard, "", 0))
	out := newLineWriter()
	in, inWriter := io.Pipe()
	go func() { _ = s.run(in, out) }()
	defer func() { _ = inWriter.Close() }()

	send(t, inWriter, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	out.next(t)
	send(t, inWriter, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	send(t, inWriter, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"edit_message","arguments":{"message_id":"1","text":"revised"}}}`)
	out.next(t)

	relay.mu.Lock()
	defer relay.mu.Unlock()
	if len(relay.edits) != 1 || relay.edits[0].Text != "revised" {
		t.Fatalf("unexpected edits recorded: %+v", relay.edits)
	}
}

func TestServer_Ping(t *testing.T) {
	relay := newFakeRelay()
	rs := relay.server()
	defer rs.Close()

	s := newServer(rs.URL, "", log.New(io.Discard, "", 0))
	out := newLineWriter()
	in, inWriter := io.Pipe()
	go func() { _ = s.run(in, out) }()
	defer func() { _ = inWriter.Close() }()

	send(t, inWriter, `{"jsonrpc":"2.0","id":9,"method":"ping"}`)
	resp := out.next(t)
	if resp["id"] != float64(9) {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func send(t *testing.T, w io.Writer, line string) {
	t.Helper()
	if _, err := io.Copy(w, strings.NewReader(line+"\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
}

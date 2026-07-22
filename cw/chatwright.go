// Package chatwright is a framework- and language-agnostic testing harness for
// conversational applications.
//
// A scenario is written once against platform-neutral verbs — send a text, expect
// a message, expect an action — and Chatwright maps them onto a concrete platform
// (Telegram today, WhatsApp next). It emulates that platform's API server,
// delivers updates to the bot's webhook over real HTTP, and captures the API
// calls the bot makes back. The bot under test may be written in any language or
// framework — Chatwright only speaks HTTP.
//
// Typical use:
//
//	cw := chatwright.New(t) // defaults to Telegram
//	// Configure the bot-under-test to use cw.BotAPIURL() as its platform API,
//	// then hand Chatwright its webhook handler (any http.Handler):
//	cw.ServeWebhook(myBot.WebhookHandler())
//
//	chat := cw.PrivateChat(chatwright.User{ID: "alice", FirstName: "Alice"})
//	chat.SendText("/start")
//	chat.ExpectBotMessage().Within(time.Second).Text("Howdy stranger")
package chatwright

import (
	"bytes"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/chatwright/chatwright/platform"
	"github.com/chatwright/chatwright/telegram"
)

// User identifies a participant in a conversation. ID is a stable handle (e.g.
// "alice"); Chatwright maps it to a deterministic per-platform numeric ID.
type User struct {
	ID        string
	FirstName string
	LastName  string
	Username  string
}

// Chatwright is a single test's conversational world: an emulated platform API
// server plus the wiring to deliver updates to the bot-under-test.
type Chatwright struct {
	t testing.TB

	platform platform.Platform
	emu      platform.Emulator

	webhookURL string
	client     *http.Client

	nextUpdateID int

	chatsMu sync.Mutex
	chats   map[int64]*Chat // cached by chatID so PrivateChat returns a stable handle per user
}

// New starts a Chatwright harness. It selects a platform (Telegram by default,
// override with OnPlatform), boots that platform's emulated API server, and
// registers cleanup with the test. Configure the bot-under-test with BotAPIURL,
// then attach its webhook via ServeWebhook or WebhookAt.
func New(t testing.TB, opts ...Option) *Chatwright {
	t.Helper()
	cw := &Chatwright{
		t:            t,
		platform:     telegram.Platform(),
		client:       http.DefaultClient,
		nextUpdateID: 1,
	}
	for _, opt := range opts {
		opt(cw)
	}
	cw.emu = cw.platform.Start()
	t.Cleanup(cw.emu.Close)
	return cw
}

// Platform is the name of the active platform, e.g. "telegram".
func (cw *Chatwright) Platform() string { return cw.platform.Name() }

// BotAPIURL is the base URL the bot-under-test must use as its platform API host,
// in place of the real one. Every call the bot makes there is captured.
func (cw *Chatwright) BotAPIURL() string { return cw.emu.BotAPIURL() }

// ServeWebhook runs the given handler as the bot-under-test's webhook on a local
// HTTP server, so updates are delivered over real HTTP. Use this for in-process
// bots. The server is shut down when the test ends.
func (cw *Chatwright) ServeWebhook(h http.Handler) {
	cw.t.Helper()
	srv := httptest.NewServer(h)
	cw.t.Cleanup(srv.Close)
	cw.webhookURL = srv.URL
}

// WebhookAt points Chatwright at an already-running bot webhook (a bot process
// started separately, in any language). Updates are POSTed to url.
func (cw *Chatwright) WebhookAt(url string) { cw.webhookURL = url }

// deliverText encodes a user's text for the active platform and POSTs it to the
// bot-under-test's webhook. The message ID is reserved from the emulator's
// shared per-chat sequence (and recorded in its journal) before encoding, so
// inbound and outbound messages in a chat never collide.
func (cw *Chatwright) deliverText(chatID int64, user platform.User, text string) {
	cw.t.Helper()
	if cw.webhookURL == "" {
		cw.t.Fatalf("chatwright: no webhook configured; call ServeWebhook or WebhookAt before sending")
		return
	}
	messageID := cw.emu.RecordInboundText(chatID, user, text)
	contentType, body := cw.emu.EncodeInboundText(platform.Inbound{
		ChatID:    chatID,
		User:      user,
		Text:      text,
		UpdateID:  cw.nextUpdateID,
		MessageID: messageID,
	})
	cw.nextUpdateID++

	cw.post(contentType, body)
}

// deliverCallback encodes a button click for the active platform and POSTs it to
// the bot-under-test's webhook.
func (cw *Chatwright) deliverCallback(chatID int64, user platform.User, data string, messageID int) {
	cw.t.Helper()
	if cw.webhookURL == "" {
		cw.t.Fatalf("chatwright: no webhook configured; call ServeWebhook or WebhookAt before clicking")
		return
	}
	cw.emu.RecordInboundCallback(chatID, user, data, messageID)
	contentType, body := cw.emu.EncodeCallback(platform.InboundCallback{
		ChatID:     chatID,
		User:       user,
		Data:       data,
		MessageID:  messageID,
		UpdateID:   cw.nextUpdateID,
		CallbackID: "cb" + strconv.Itoa(cw.nextUpdateID),
	})
	cw.nextUpdateID++
	cw.post(contentType, body)
}

// post delivers an encoded webhook payload to the bot-under-test.
func (cw *Chatwright) post(contentType string, body []byte) {
	cw.t.Helper()
	resp, err := cw.client.Post(cw.webhookURL, contentType, bytes.NewReader(body))
	if err != nil {
		cw.t.Fatalf("chatwright: deliver update to webhook: %v", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		cw.t.Fatalf("chatwright: webhook returned status %d", resp.StatusCode)
	}
}

// userChatID maps a string user handle to a stable positive int64 platform ID.
func userChatID(handle string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(handle))
	id := int64(h.Sum64() & 0x7fffffffffff) // keep it positive and human-sized
	if id == 0 {
		id = 1
	}
	return id
}

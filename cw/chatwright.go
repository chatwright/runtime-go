// Package chatwright is a framework- and language-agnostic testing harness for
// conversational applications.
//
// A scenario is written once against platform-neutral verbs — send a text, expect
// a message, expect an action — and Chatwright maps them onto a concrete platform
// (Telegram today, WhatsApp next). It emulates that platform's API server, which
// owns delivering updates to the bot-under-test (over a real HTTP webhook, or via
// getUpdates long-polling on platforms that support it) and captures the API
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
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/chatwright/chatwright/platform"
	"github.com/chatwright/chatwright/telegram"
)

// defaultSafetyTimeout is the wall-clock ceiling Chatwright waits for a bot
// reply before failing a test, unless overridden with WithSafetyTimeout. It
// is independent of any per-assertion Within budget: Within never shortens
// the observation window, only the latency a reply is judged against once it
// arrives. See BotMessage.Within.
const defaultSafetyTimeout = 5 * time.Second

// User identifies a participant in a conversation. ID is a stable handle (e.g.
// "alice"); Chatwright maps it to a deterministic per-platform numeric ID.
type User struct {
	ID        string
	FirstName string
	LastName  string
	Username  string
}

// Chatwright is a single test's conversational world: an emulated platform API
// server plus the wiring to attach the bot-under-test's webhook (when it has
// one). The emulator — not Chatwright — owns building updates, assigning them
// identity, and delivering them; Chatwright only submits neutral actions.
type Chatwright struct {
	t testing.TB

	platform platform.Platform
	emu      platform.Emulator

	client *http.Client

	safetyTimeout time.Duration

	chatsMu sync.Mutex
	chats   map[int64]*Chat // cached by chatID so PrivateChat returns a stable handle per user
}

// New starts a Chatwright harness. It selects a platform (Telegram by default,
// override with OnPlatform), boots that platform's emulated API server, and
// registers cleanup with the test. Configure the bot-under-test with BotAPIURL,
// then attach its webhook via ServeWebhook or WebhookAt — or, on platforms that
// support it (Telegram), leave neither set and run the bot's own getUpdates
// polling loop against BotAPIURL instead.
func New(t testing.TB, opts ...Option) *Chatwright {
	t.Helper()
	cw := &Chatwright{
		t:             t,
		platform:      telegram.Platform(),
		client:        http.DefaultClient,
		safetyTimeout: defaultSafetyTimeout,
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
	cw.emu.SetWebhook(srv.URL, cw.client)
}

// WebhookAt points Chatwright at an already-running bot webhook (a bot process
// started separately, in any language). The emulator POSTs updates to url.
func (cw *Chatwright) WebhookAt(url string) { cw.emu.SetWebhook(url, cw.client) }

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

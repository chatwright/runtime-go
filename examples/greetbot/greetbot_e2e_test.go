package greetbot_test

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/chatwright/chatwright/chatwrite"
	"github.com/chatwright/chatwright/chatwrite/examples/greetbot"
)

// TestGreetBotEndToEnd runs greetScenario against a full end-to-end setup:
//
//  1. Chatwright boots an emulated Telegram Bot API server.
//  2. A real greetbot is constructed to use that emulator as its Bot API host.
//  3. The bot is served on its own HTTP listener (a real TCP port).
//  4. The scenario is connected to that listener with WebhookAt, and driven with
//     platform-neutral verbs.
//
// Nothing is stubbed: the bot parses real Telegram updates and replies via the
// real tgbotapi client; Chatwright delivers updates and captures the API calls
// over HTTP.
func TestGreetBotEndToEnd(t *testing.T) {
	_, chat := startGreetBot(t)

	greetScenario(chat)
}

// greetScenario is the platform-neutral happy path: greet, then inspect
// /start's language options.
func greetScenario(chat *chatwright.Chat) {
	chat.SendText("Hi")
	chat.ExpectBotMessage().
		Within(time.Second).
		Text("Howdy stranger")

	chat.SendText("/start")
	chat.ExpectBotMessage().
		Within(time.Second).
		IsTextMessage().
		ExpectAction(0, 0).
		Label("English").
		ID("lang:en")
}

// TestGreetBotLanguageSelection drives /start, clicks a non-default language
// button, and validates the bot replies in the selected language from then on —
// both immediately after selection (by editing the language-choice message in
// place) and on a later, unrelated message.
func TestGreetBotLanguageSelection(t *testing.T) {
	_, chat := startGreetBot(t)

	chat.SendText("/start")
	msg := chat.ExpectBotMessage().
		Within(time.Second).
		IsTextMessage()
	msg.Text("Choose your language")

	// Pick Español (row 1: en, es, fr -> es is row 1). The bot edits THIS
	// message in place, translating it, instead of sending a new one.
	msg.ExpectAction(1, 0).
		Label("Español").
		ID("lang:es").
		Click()

	msg.ExpectEdited().
		Within(time.Second).
		Text("¡Hola, forastero!")

	// The selection is remembered: a later, unrelated message still gets the
	// Spanish greeting, not the English default.
	chat.SendText("Hi again").
		ExpectBotMessage().
		Within(time.Second).
		Text("¡Hola, forastero!")
}

// TestGreetBotTime runs timeScenario: a scenario dedicated to /time, decoupled
// from greeting/language concerns, against a fixed clock so the reply is exactly
// assertable instead of racing the wall clock.
func TestGreetBotTime(t *testing.T) {
	fixedNow := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	_, chat := startGreetBot(t, greetbot.WithClock(func() time.Time { return fixedNow }))

	timeScenario(chat, fixedNow)
}

// timeScenario validates /time in isolation: the bot must return exactly what
// its clock says, formatted the way greetbot.FormatTime defines.
func timeScenario(chat *chatwright.Chat, wantNow time.Time) {
	chat.SendText("/time")
	chat.ExpectBotMessage().
		Within(time.Second).
		Text(greetbot.FormatTime(wantNow))
}

// startGreetBot boots Chatwright, runs a real greetbot (configured with opts) on
// its own TCP listener, and connects the scenario to it via WebhookAt.
func startGreetBot(t *testing.T, opts ...greetbot.Option) (*chatwright.Chatwright, *chatwright.Chat) {
	t.Helper()
	cw := chatwright.New(t) // Telegram is the default platform

	const token = "TEST:TOKEN"
	bot := greetbot.New(cw.BotAPIURL(), token, opts...)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/webhook", bot.Handler())
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	cw.WebhookAt("http://" + ln.Addr().String() + "/webhook")

	chat := cw.PrivateChat(chatwright.User{ID: "alice", FirstName: "Alice"})
	return cw, chat
}

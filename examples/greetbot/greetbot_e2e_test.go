package greetbot_test

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/chatwright/chatwright"
	"github.com/chatwright/chatwright/examples/greetbot"
)

// TestGreetBotEndToEnd runs a full end-to-end loop against a real bot:
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
	cw := chatwright.New(t) // Telegram is the default platform

	const token = "TEST:TOKEN"
	bot := greetbot.New(cw.BotAPIURL(), token)

	// Start the bot's own HTTP listener on a random local port.
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

	// Connect the scenario to the running bot over HTTP.
	cw.WebhookAt("http://" + ln.Addr().String() + "/webhook")

	greetScenario(cw)
}

// greetScenario is platform-neutral: send a greeting, expect a greeting back,
// then check the /start message offers the expected inline action.
func greetScenario(cw *chatwright.Chatwright) {
	chat := cw.PrivateChat(chatwright.User{ID: "alice", FirstName: "Alice"})

	chat.SendText("Hi")
	chat.ExpectBotMessage().
		Within(time.Second).
		Text("Howdy stranger")

	chat.SendText("/start")
	chat.ExpectBotMessage().
		Within(time.Second).
		IsTextMessage().
		ExpectAction(0, 0).
		Label("My events").
		ID("my-events")
}

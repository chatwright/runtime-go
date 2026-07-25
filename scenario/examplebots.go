package scenario

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"chatwright.dev/runtime/examples/greetbot"
	"chatwright.dev/runtime/telegram"
)

// exampleBotToken is the fake bot token this package's own example-bot boot
// uses — never a real credential; the Telegram emulator records but never
// validates it, mirroring arena.greetbotToken.
const exampleBotToken = "TEST:TOKEN"

// ExampleBotSession is what booting one exampleBot produced: the emulator
// it is wired to, and a Close tearing both down. Mirrors
// arena.ScenarioSession's Setup/Close shape.
type ExampleBotSession struct {
	Emulator *telegram.Emulator
	Close    func()
}

// ExampleBotFactory boots one fresh exampleBot session. Every call gets
// fresh state — no two Build calls, and no Build call and a warm-up, ever
// observe another's conversation history — matching arena's own
// Scenario.Setup contract.
type ExampleBotFactory func() (*ExampleBotSession, error)

// ExampleBotRegistry maps an exampleBot id (Bot.ExampleBot) to the factory
// that boots it. Not an extension point (see Bot.ExampleBot's own doc
// comment in document.go): this registry's key set is exactly
// DefaultSupportedExampleBots, the set validate checks a document's
// `requires: ["exampleBot:<id>"]` declaration against.
type ExampleBotRegistry map[string]ExampleBotFactory

// DefaultExampleBots returns the registry this runtime (runtime-go) ships:
// "greetbot", booting chatwright.dev/runtime/examples/greetbot over a fresh
// telegram.Emulator — exactly arena.GreetbotScenario()'s own setupGreetbot.
func DefaultExampleBots() ExampleBotRegistry {
	return ExampleBotRegistry{"greetbot": bootGreetbot}
}

func bootGreetbot() (*ExampleBotSession, error) {
	emu := telegram.NewEmulator()
	bot := greetbot.New(emu.BotAPIURL(), exampleBotToken)
	srv := httptest.NewServer(bot.Handler())
	emu.SetWebhook(srv.URL, http.DefaultClient)
	return &ExampleBotSession{
		Emulator: emu,
		Close: func() {
			srv.Close()
			emu.Close()
		},
	}, nil
}

// Boot resolves id against r, returning a clear error naming id when
// unregistered. validate's requires check should already have refused an
// undeclared exampleBot id before Build ever runs — reaching an unknown id
// here means the running binary's registry disagrees with
// DefaultSupportedExampleBots, a packaging bug rather than a document
// problem.
func (r ExampleBotRegistry) Boot(id string) (*ExampleBotSession, error) {
	factory, ok := r[id]
	if !ok {
		return nil, fmt.Errorf("scenario: no exampleBot registered for %q", id)
	}
	return factory()
}

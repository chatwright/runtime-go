# chatwright.dev/runtime

The [Chatwright](https://chatwright.dev) engine: platform emulation and the
testing runtime for conversational applications.

Its sibling repository,
[runtime-ts](https://github.com/chatwright/runtime-ts), is the browser
runtime — published to npm as `@chatwright/runtime`, just as this
repository (runtime-go) publishes the Go module `chatwright.dev/runtime`.
It is the orchestrator behind the Studio Playground, currently a scaffold
per
[decision 0012](https://github.com/chatwright/chatwright/blob/main/spec/decisions/0012-black-box-bot-protocol.md).
The two runtimes share language-independent contracts — the
[run-bundle v1 format](https://chatwright.dev/formats/run-bundle/v1) and the
black-box bot protocol — never code; conformance is proven by shared
fixtures.

This module is where a Chatwright run actually happens. It emulates a chat
platform's API server (Telegram first; the WhatsApp surface is present),
delivers updates to the bot-under-test over real HTTP, captures everything
the bot sends back into an append-only per-chat journal, and drives both
deterministic scenarios and AI-goal exploration over that shared journal:

- `cw` — the scenario API: platform-neutral verbs (`SendText`,
  `ExpectBotMessage`, `ExpectAction`, …) bound to a `testing.TB`, plus
  scenario fragments and execution-context provenance.
- `telegram`, `whatsapp`, `platform` — the emulated platform servers and the
  neutral contracts they implement.
- `observe`, `goal`, `actor`, `campaign`, `datastate`, `branching` — the
  observation engine, goal/task contracts, the actor loop, campaign report
  assembly, data-state assertions and branch exploration.
- `run` — part composition (hybrid runs) and run-bundle assembly: it converts
  this runtime's internal records into the wire types
  [chatwright.dev/sdk](https://github.com/chatwright/sdk-go) owns.

The bot under test may be written in any language or framework — Chatwright
only speaks HTTP (see `examples/pybot` for a Python bot driven as a real
subprocess).

## Install

```sh
go get chatwright.dev/runtime
```

## Usage

```go
package mybot_test

import (
	"testing"
	"time"

	"chatwright.dev/runtime/cw"
)

func TestGreeting(t *testing.T) {
	w := cw.New(t) // boots an emulated Telegram Bot API server
	w.ServeWebhook(myBot.WebhookHandler())

	chat := w.PrivateChat(cw.User{ID: "alice", FirstName: "Alice"})
	chat.SendText("Hi")
	chat.ExpectBotMessage().Within(time.Second).Text("Howdy stranger")
}
```

A complete, runnable version of this flow — a real bot on its own TCP
listener, webhook delivery, language selection via inline buttons and
in-place message edits — lives in
[`examples/greetbot`](examples/greetbot).

## Dependency rule

The runtime depends on [chatwright.dev/sdk](https://github.com/chatwright/sdk-go),
never the reverse. The sdk owns every run-bundle wire type; this module
produces bundles by converting its internal records to those types (see
`run/wire.go`) and never redefines a wire shape of its own.

## The standard

Specs, format documentation and design decisions live in the standard
repository, [github.com/chatwright/chatwright](https://github.com/chatwright/chatwright),
and at [chatwright.dev](https://chatwright.dev). The run-bundle wire model is
[github.com/chatwright/sdk-go](https://github.com/chatwright/sdk-go).

## Licence

Apache-2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).

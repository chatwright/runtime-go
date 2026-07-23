package cw

import (
	"net/http"
	"time"

	"chatwright.dev/runtime/platform"
)

// Option configures a Chatwright harness at construction time.
type Option func(*Chatwright)

// OnPlatform selects the platform a scenario runs against, e.g.
// cw.OnPlatform(whatsapp.Platform()). Defaults to Telegram.
func OnPlatform(p platform.Platform) Option {
	return func(cw *Chatwright) { cw.platform = p }
}

// WithHTTPClient overrides the HTTP client used to deliver updates to the bot's
// webhook. Rarely needed; useful for custom timeouts or transports.
func WithHTTPClient(c *http.Client) Option {
	return func(cw *Chatwright) { cw.client = c }
}

// WithWebhookHandler attaches the bot-under-test's webhook handler at
// construction time, equivalent to calling ServeWebhook after New.
func WithWebhookHandler(h http.Handler) Option {
	return func(cw *Chatwright) { cw.ServeWebhook(h) }
}

// WithSafetyTimeout overrides the wall-clock ceiling Chatwright waits for a
// bot reply before failing a test (default 5s). It applies to every
// BotMessage wait regardless of any per-assertion Within budget: Within
// records a latency budget asserted once a reply arrives, but never shortens
// how long Chatwright is willing to keep listening. Lower it in fast, tight
// test suites to fail sooner when a bot never replies at all; raise it if the
// bot-under-test is intrinsically slow (e.g. it calls a real, unmocked
// external service).
func WithSafetyTimeout(d time.Duration) Option {
	return func(cw *Chatwright) { cw.safetyTimeout = d }
}

// WithListenAddr binds the emulated platform API server to a caller-chosen
// local address (e.g. "127.0.0.1:54321") instead of a random port.
//
// The common case — ServeWebhook driving an in-process bot — never needs
// this: cw.BotAPIURL() is available as soon as New returns, before the bot
// is even constructed. It matters for a bot-under-test started as a
// separate process, in any language, since Chatwright only speaks HTTP:
// that process reads its API base URL from its own configuration (e.g. an
// environment variable) at start-up, so the address must be decided before
// New runs, not read back from it afterwards. Pick a free address once
// (e.g. bind to "127.0.0.1:0", read the assigned port, then close it),
// configure the process with it, and pass the same address here — the
// emulator then binds exactly where the process already expects it,
// regardless of which of the two is started first. See examples/pybot for a
// complete non-Go example using this seam.
//
// Only platforms that implement platform.AddrPlatform support this
// (Telegram does); New fails the test via t.Fatalf if a non-empty address is
// set for a platform that doesn't, and if the address itself cannot be
// bound (e.g. already in use).
func WithListenAddr(addr string) Option {
	return func(cw *Chatwright) { cw.listenAddr = addr }
}

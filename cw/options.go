package chatwright

import (
	"net/http"
	"time"

	"github.com/chatwright/chatwright/platform"
)

// Option configures a Chatwright harness at construction time.
type Option func(*Chatwright)

// OnPlatform selects the platform a scenario runs against, e.g.
// chatwright.OnPlatform(whatsapp.Platform()). Defaults to Telegram.
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

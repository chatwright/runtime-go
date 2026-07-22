package chatwright

import (
	"net/http"

	"github.com/chatwright/chatwright/chatwrite/platform"
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

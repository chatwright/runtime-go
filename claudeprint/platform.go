// Package claudeprint implements a Claude Code Platform for Chatwright that
// drives `claude` in print mode (`claude -p ... --output-format
// stream-json`) instead of the interactive channel contract claudechannel
// uses.
//
// claudechannel exists first and is the more capable design on paper — one
// long-running claude session, driven over the claude/channel MCP contract,
// so a scenario can send several turns into the same conversation the way a
// human would type into a chat. In practice that design is blocked: as of
// claude 2.1.263, a manually configured ("server:<name>") channel is
// rejected by a platform-side approved-channels allowlist even with
// --dangerously-load-development-channels, so no message ever reaches the
// model (see claudechannel's package doc, "Known limitation").
//
// claudeprint sidesteps the blocker by never opening a channel at all: each
// user turn spawns its own `claude -p <text> --output-format stream-json
// --verbose ...` process, non-interactive from the start, so there is no
// allowlist gate to trip. The trade-off is real — one OS process per turn
// instead of one long-running session, no interactive-action (button)
// support, and multi-turn continuity depends on `--resume <session-id>`
// re-attaching to the same server-side conversation rather than a live
// process holding it in memory — but it works today, against the current
// claude binary, with no known gate blocking it.
//
// # Session continuity
//
// Emulator generates one UUID per chat the first time SubmitText is called
// for it, passes it as `--session-id <uuid>` on that chat's first turn, and
// as `--resume <uuid>` on every later turn — the same session ID, so claude
// resumes the prior turn's server-side conversation state instead of
// starting fresh.
//
// # Environment
//
// Every claude process claudeprint spawns runs with a copy of the current
// process environment from which every CLAUDE*-prefixed variable has been
// removed before Options.Env is applied on top — the same
// nested-session-detection leak documented in claudechannel's package doc
// (sanitizedEnviron): when claudeprint's own test suite runs inside a
// claude session, CLAUDECODE and friends would otherwise leak into the
// child by plain os.Environ() inheritance. ANTHROPIC_*-prefixed variables
// (API auth) are left untouched.
package claudeprint

import "chatwright.dev/runtime/platform"

// New returns the claudeprint Platform for use with cw.OnPlatform. The
// bot-under-test is a real `claude` process (or, in tests, a stand-in
// binary set with WithClaudeBinary) spawned fresh for every user turn.
func New(opts ...Option) platform.Platform {
	return cpPlatform{opts: resolve(opts)}
}

type cpPlatform struct {
	opts Options
}

func (cpPlatform) Name() string { return "claudeprint" }

func (p cpPlatform) Start() platform.Emulator { return NewEmulator(p.opts) }

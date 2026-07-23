package cw

import (
	"regexp"
	"strings"
	"time"

	"chatwright.dev/runtime/platform"
)

// BotMessage is a fluent handle to a message the bot sent. Assertion methods
// (Text, IsTextMessage, ExpectAction, ...) block until the message arrives —
// up to the harness's safety timeout (see WithSafetyTimeout), regardless of
// any Within budget — and fail the test if it never does. They are
// platform-neutral.
type BotMessage struct {
	chat *Chat

	// enforceWithin and within hold the latency budget set by Within, if any.
	// This is an assertion checked once a reply arrives, not a wait window:
	// Within never shortens how long Chatwright keeps listening.
	enforceWithin bool
	within        time.Duration

	resolved bool
	msg      *platform.Message
	latency  time.Duration

	// editOf, when set, makes resolve wait for an in-place edit of that message
	// (Version > editOf.Version) instead of waiting for a new outbound message.
	editOf *platform.Message
}

// Within sets the latency budget a reply is judged against: once a reply
// arrives, if it took longer than d, the test fails showing the observed
// latency and the reply's actual text. Within does NOT shorten how long
// Chatwright waits for that reply to arrive in the first place — that ceiling
// is the harness's safety timeout (default 5s; see WithSafetyTimeout),
// independent of d, so a late-but-arrived reply is a diagnostic failure
// (expected/actual text, observed latency) rather than an opaque "none
// arrived" timeout. If d exceeds the configured safety timeout, the wait is
// extended to d so a generous budget is never undercut by it.
//
// Calling Within after the message has already resolved (e.g. after Text or
// IsTextMessage) is a usage error: the wait already happened, so a budget set
// now can no longer be honored. It fails the test immediately with a clear
// message instead of silently doing nothing.
func (m *BotMessage) Within(d time.Duration) *BotMessage {
	m.chat.cw.t.Helper()
	if m.resolved {
		m.chat.cw.t.Fatalf("chatwright: Within called after the message was already resolved; call Within before Text/IsTextMessage/ExpectAction/Metrics/Snapshot")
		return m
	}
	m.enforceWithin = true
	m.within = d
	return m
}

// resolve waits for the bot's next message to this chat (or, for a handle
// obtained via ExpectEdited, for that specific message to be edited), and
// records latency.
func (m *BotMessage) resolve() {
	if m.resolved {
		return
	}
	m.chat.cw.t.Helper()

	wait := m.chat.cw.safetyTimeout
	if m.enforceWithin && m.within > wait {
		wait = m.within
	}

	var msg *platform.Message
	var ok bool
	if m.editOf != nil {
		msg, ok = m.chat.cw.emu.WaitForEdit(m.chat.chatID, m.editOf.MessageID, m.editOf.Version, wait)
		if !ok {
			m.chat.cw.t.Fatalf("chatwright: expected message %d to be edited within %s (safety timeout), but it was not\n%s",
				m.editOf.MessageID, wait, m.chat.cw.emu.Transcript(m.chat.chatID))
			return
		}
	} else {
		msg, ok = m.chat.cw.emu.WaitForMessage(m.chat.chatID, m.chat.consumed, wait)
		if !ok {
			m.chat.cw.t.Fatalf("chatwright: expected a bot message within %s (safety timeout), but none arrived\n%s",
				wait, m.chat.cw.emu.Transcript(m.chat.chatID))
			return
		}
		m.chat.consumed++
	}
	m.msg = msg
	m.resolved = true

	if lat := msg.ReceivedAt.Sub(m.chat.lastSent); lat > 0 {
		m.latency = lat
	}
	if m.enforceWithin && m.latency > m.within {
		m.chat.cw.t.Errorf("chatwright: reply arrived after %s, budget %s: %q", m.latency, m.within, msg.Text)
	}
}

// IsTextMessage asserts the bot's message carries text, returning the handle for
// further assertions.
func (m *BotMessage) IsTextMessage() *BotMessage {
	m.chat.cw.t.Helper()
	m.resolve()
	if m.msg.Text == "" {
		m.chat.cw.t.Errorf("chatwright: expected a text message, but text was empty")
	}
	return m
}

// Text asserts the bot's message text equals want.
func (m *BotMessage) Text(want string) *BotMessage {
	m.chat.cw.t.Helper()
	m.resolve()
	if m.msg.Text != want {
		m.chat.cw.t.Errorf("chatwright: bot message text = %q, want %q", m.msg.Text, want)
	}
	return m
}

// TextContains asserts the bot's message text contains substr.
func (m *BotMessage) TextContains(substr string) *BotMessage {
	m.chat.cw.t.Helper()
	m.resolve()
	if !strings.Contains(m.msg.Text, substr) {
		m.chat.cw.t.Errorf("chatwright: bot message text = %q, want it to contain %q", m.msg.Text, substr)
	}
	return m
}

// TextMatches asserts the bot's message text matches the given regular
// expression (as accepted by the regexp package). An invalid pattern fails the
// test immediately rather than silently matching nothing.
func (m *BotMessage) TextMatches(pattern string) *BotMessage {
	m.chat.cw.t.Helper()
	re, err := regexp.Compile(pattern)
	if err != nil {
		m.chat.cw.t.Fatalf("chatwright: TextMatches: invalid pattern %q: %v", pattern, err)
		return m
	}
	m.resolve()
	if !re.MatchString(m.msg.Text) {
		m.chat.cw.t.Errorf("chatwright: bot message text = %q, want it to match %q", m.msg.Text, pattern)
	}
	return m
}

// ExpectEdited returns a fluent handle that waits for this message to be
// edited in place (e.g. a Telegram editMessageText call) and asserts on its new
// content — the same assertion methods as ExpectBotMessage, but bound to this
// message's identity rather than to the next outbound message. Like
// ExpectBotMessage, it waits up to the harness's safety timeout and starts
// with no latency budget of its own; add one with Within.
func (m *BotMessage) ExpectEdited() *BotMessage {
	m.chat.cw.t.Helper()
	m.resolve()
	return m.chat.newBotMessage(m.msg)
}

// Metrics returns the metrics captured for this message.
func (m *BotMessage) Metrics() Metrics {
	m.chat.cw.t.Helper()
	m.resolve()
	return Metrics{Latency: m.latency}
}

// Snapshot returns an immutable observation of the resolved bot message.
// Nested action rows are detached from the emulator's mutable message state,
// so callers can inspect transport output without changing later assertions.
func (m *BotMessage) Snapshot() platform.Message {
	m.chat.cw.t.Helper()
	m.resolve()
	snapshot := *m.msg
	snapshot.Actions = make([][]platform.Action, len(m.msg.Actions))
	for i, row := range m.msg.Actions {
		snapshot.Actions[i] = append([]platform.Action(nil), row...)
	}
	return snapshot
}

// Metrics are first-class measurements captured for a bot message.
type Metrics struct {
	Latency time.Duration
}

// action returns the neutral action at (row, col), failing the test if absent.
func (m *BotMessage) action(row, col int) *platform.Action {
	m.chat.cw.t.Helper()
	m.resolve()
	rows := m.msg.Actions
	if row < 0 || row >= len(rows) || col < 0 || col >= len(rows[row]) {
		m.chat.cw.t.Fatalf("chatwright: expected an action at [%d,%d], but it is missing\n%s",
			row, col, m.chat.cw.emu.Transcript(m.chat.chatID))
		return nil
	}
	return &rows[row][col]
}

// ExpectAction returns a platform-neutral handle to the interactive action
// (button) at (row, col). Use Label and ID — these map onto each platform's
// native representation (Telegram button text/callback_data, WhatsApp reply
// title/id).
func (m *BotMessage) ExpectAction(row, col int) *Action {
	m.chat.cw.t.Helper()
	m.resolve()
	return &Action{chat: m.chat, act: m.action(row, col), messageID: m.msg.MessageID}
}

// Action is a platform-neutral assertion handle for an interactive action.
type Action struct {
	chat      *Chat
	act       *platform.Action
	messageID int
}

// Click activates the action, sending the appropriate event back to the bot: a
// callback for actions with an ID (Telegram callback query / WhatsApp interactive
// reply), or the action's label as text otherwise. Returns the chat so a reply
// can be asserted next.
func (a *Action) Click() *Chat {
	a.chat.cw.t.Helper()
	a.chat.lastSent = time.Now()
	var err error
	if a.act.ID != "" {
		err = a.chat.cw.emu.SubmitClick(a.chat.chatID, a.chat.user, a.act.ID, a.messageID)
	} else {
		err = a.chat.cw.emu.SubmitText(a.chat.chatID, a.chat.user, a.act.Label)
	}
	if err != nil {
		a.chat.cw.t.Fatalf("chatwright: %v", err)
	}
	return a.chat
}

// Label asserts the action's user-visible label.
func (a *Action) Label(want string) *Action {
	a.chat.cw.t.Helper()
	if a.act.Label != want {
		a.chat.cw.t.Errorf("chatwright: action label = %q, want %q", a.act.Label, want)
	}
	return a
}

// ID asserts the action's stable identifier.
func (a *Action) ID(want string) *Action {
	a.chat.cw.t.Helper()
	if a.act.ID != want {
		a.chat.cw.t.Errorf("chatwright: action id = %q, want %q", a.act.ID, want)
	}
	return a
}

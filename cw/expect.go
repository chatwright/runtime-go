package chatwright

import (
	"time"

	"github.com/chatwright/chatwright/platform"
)

// BotMessage is a fluent handle to a message the bot sent. Assertion methods
// (Text, IsTextMessage, ExpectAction, ...) block until the message arrives (up to
// the wait window) and fail the test if it does not. They are platform-neutral.
type BotMessage struct {
	chat    *Chat
	timeout time.Duration

	enforceWithin bool
	within        time.Duration

	resolved bool
	msg      *platform.Message
	latency  time.Duration
}

// Within bounds how long the bot may take to reply. It both sets the wait window
// and asserts (via the captured latency metric) that the reply arrived in time.
func (m *BotMessage) Within(d time.Duration) *BotMessage {
	m.timeout = d
	m.enforceWithin = true
	m.within = d
	return m
}

// resolve waits for the bot's next message to this chat and records latency.
func (m *BotMessage) resolve() {
	if m.resolved {
		return
	}
	m.chat.cw.t.Helper()
	msg, ok := m.chat.cw.emu.WaitForMessage(m.chat.chatID, m.chat.consumed, m.timeout)
	if !ok {
		m.chat.cw.t.Fatalf("chatwright: expected a bot message within %s, but none arrived", m.timeout)
		return
	}
	m.msg = msg
	m.chat.consumed++
	m.resolved = true

	if lat := msg.ReceivedAt.Sub(m.chat.lastSent); lat > 0 {
		m.latency = lat
	}
	if m.enforceWithin && m.latency > m.within {
		m.chat.cw.t.Errorf("chatwright: bot replied in %s, want within %s", m.latency, m.within)
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

// Metrics returns the metrics captured for this message.
func (m *BotMessage) Metrics() Metrics {
	m.resolve()
	return Metrics{Latency: m.latency}
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
		m.chat.cw.t.Fatalf("chatwright: expected an action at [%d,%d], but it is missing", row, col)
		return nil
	}
	return &rows[row][col]
}

// ExpectAction returns a platform-neutral handle to the interactive action
// (button) at (row, col). Use Label and ID — these map onto each platform's
// native representation (Telegram button text/callback_data, WhatsApp reply
// title/id).
func (m *BotMessage) ExpectAction(row, col int) *Action {
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
	if a.act.ID != "" {
		a.chat.cw.deliverCallback(a.chat.chatID, a.chat.user, a.act.ID, a.messageID)
	} else {
		a.chat.cw.deliverText(a.chat.chatID, a.chat.user, a.act.Label)
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

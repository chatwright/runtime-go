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

	// editOf, when set, makes resolve wait for an in-place edit of that message
	// (Version > editOf.Version) instead of waiting for a new outbound message.
	editOf *platform.Message
}

// Within bounds how long the bot may take to reply. It both sets the wait window
// and asserts (via the captured latency metric) that the reply arrived in time.
func (m *BotMessage) Within(d time.Duration) *BotMessage {
	m.timeout = d
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

	var msg *platform.Message
	var ok bool
	if m.editOf != nil {
		msg, ok = m.chat.cw.emu.WaitForEdit(m.chat.chatID, m.editOf.MessageID, m.editOf.Version, m.timeout)
		if !ok {
			m.chat.cw.t.Fatalf("chatwright: expected message %d to be edited within %s, but it was not", m.editOf.MessageID, m.timeout)
			return
		}
	} else {
		msg, ok = m.chat.cw.emu.WaitForMessage(m.chat.chatID, m.chat.consumed, m.timeout)
		if !ok {
			m.chat.cw.t.Fatalf("chatwright: expected a bot message within %s, but none arrived", m.timeout)
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

// ExpectEdited returns a fluent handle that waits for this message to be
// edited in place (e.g. a Telegram editMessageText call) and asserts on its new
// content — the same assertion methods as ExpectBotMessage, but bound to this
// message's identity rather than to the next outbound message. The wait window
// defaults to this handle's own (5s unless overridden); narrow it with Within.
func (m *BotMessage) ExpectEdited() *BotMessage {
	m.chat.cw.t.Helper()
	m.resolve()
	return &BotMessage{chat: m.chat, timeout: m.timeout, editOf: m.msg}
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

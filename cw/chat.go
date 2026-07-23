package cw

import (
	"time"

	"chatwright.dev/runtime/platform"
)

// Chat is a conversation between a user and the bot-under-test. Obtain one via
// Chatwright.PrivateChat, then drive it with SendText and assert with
// ExpectBotMessage. Its methods are platform-neutral.
type Chat struct {
	cw     *Chatwright
	chatID int64
	user   platform.User

	consumed int       // how many bot messages to this chat have been asserted
	lastSent time.Time // when the most recent inbound update was delivered (for latency)
}

// PrivateChat returns the private chat between the given user and the bot.
// Calling it again for the same user returns the same *Chat handle, not a fresh
// one: the consumption cursor (which bot messages have already been asserted
// on) and lastSent latency baseline are shared across every call site that asks
// for that user's chat, matching Telegram's one chat per user.
func (cw *Chatwright) PrivateChat(u User) *Chat {
	id := userChatID(u.ID)

	cw.chatsMu.Lock()
	defer cw.chatsMu.Unlock()
	if cw.chats == nil {
		cw.chats = make(map[int64]*Chat)
	}
	if c, ok := cw.chats[id]; ok {
		return c
	}
	c := &Chat{
		cw:     cw,
		chatID: id,
		user: platform.User{
			ID:        id,
			FirstName: u.FirstName,
			LastName:  u.LastName,
			Username:  u.Username,
		},
	}
	cw.chats[id] = c
	return c
}

// SendText delivers a text message from the user to the bot-under-test. The
// emulator builds the platform-native update and delivers it — over the
// bot's webhook, or by queuing it for getUpdates on platforms that support
// polling — Chatwright itself never touches the wire.
func (c *Chat) SendText(text string) *Chat {
	c.cw.t.Helper()
	c.lastSent = time.Now()
	if err := c.cw.emu.SubmitText(c.chatID, c.user, text); err != nil {
		c.cw.t.Fatalf("chatwright: %v", err)
	}
	return c
}

// ExpectBotMessage asserts that the bot sends a message to this chat, returning a
// fluent handle to assert on its content. Chatwright waits up to the harness's
// safety timeout (default 5s; see WithSafetyTimeout) for it to arrive; narrow
// the latency this is judged against — without shortening that wait — with
// Within.
func (c *Chat) ExpectBotMessage() *BotMessage {
	return c.newBotMessage(nil)
}

// ExpectNoMessage asserts that the bot does not send a new message to this chat
// within the given window. It fails the test if one arrives, reporting its text.
// Unlike ExpectBotMessage, it does not consume a slot in the chat's message
// cursor: a subsequent ExpectBotMessage still waits for the next unconsumed
// message.
func (c *Chat) ExpectNoMessage(within time.Duration) {
	c.cw.t.Helper()
	msg, ok := c.cw.emu.WaitForMessage(c.chatID, c.consumed, within)
	if ok {
		c.cw.t.Errorf("chatwright: expected no bot message within %s, but got %q\n%s",
			within, msg.Text, c.cw.emu.Transcript(c.chatID))
	}
}

// newBotMessage constructs a BotMessage handle and registers a cleanup check:
// if the test ends without the expectation ever being resolved (no assertion
// method — Text, IsTextMessage, ExpectAction, Metrics, Snapshot, ... — was
// called on it), the test fails. A bare ExpectBotMessage() call that nothing
// ever asserts on is a silent no-op otherwise, masking bugs the scenario meant
// to catch.
func (c *Chat) newBotMessage(editOf *platform.Message) *BotMessage {
	m := &BotMessage{chat: c, editOf: editOf}
	c.cw.t.Cleanup(func() {
		if !m.resolved {
			c.cw.t.Errorf("chatwright: a BotMessage expectation was never resolved (call Text, IsTextMessage, ExpectAction, Metrics, or Snapshot on it, or remove it)")
		}
	})
	return m
}

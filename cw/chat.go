package chatwright

import (
	"time"

	"github.com/chatwright/chatwright/platform"
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
// Calling it again for the same user returns a chat with the same stable IDs.
func (cw *Chatwright) PrivateChat(u User) *Chat {
	id := userChatID(u.ID)
	return &Chat{
		cw:     cw,
		chatID: id,
		user: platform.User{
			ID:        id,
			FirstName: u.FirstName,
			LastName:  u.LastName,
			Username:  u.Username,
		},
	}
}

// SendText delivers a text message from the user to the bot-under-test. Chatwright
// encodes it for the active platform and POSTs it to the bot's webhook.
func (c *Chat) SendText(text string) *Chat {
	c.cw.t.Helper()
	c.lastSent = time.Now()
	c.cw.deliverText(c.chatID, c.user, text)
	return c
}

// ExpectBotMessage asserts that the bot sends a message to this chat, returning a
// fluent handle to assert on its content. The wait window defaults to 5s; narrow
// it with Within.
func (c *Chat) ExpectBotMessage() *BotMessage {
	return &BotMessage{chat: c, timeout: 5 * time.Second}
}

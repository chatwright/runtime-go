package actor_test

import (
	"sync"
	"time"

	"chatwright.dev/runtime/platform"
)

// fakeChat is a minimal, fully controlled stand-in for a live
// platform.Emulator: it implements both observe.Journaler (Journal) and
// actor.Actuator (SubmitText/SubmitClick/WaitForMessage/WaitForEdit) over
// one shared, in-memory conversation, so loop unit tests can script exact
// bot replies — or exact non-replies, to exercise no-effect/non-progress
// paths — without a real platform emulator underneath. The real Telegram
// emulator is exercised separately, in the end-to-end gate test.
//
// Bot behaviour is scripted with queueBotReply/queueNoReply, consumed FIFO
// by the next SubmitText or SubmitClick call.
type fakeChat struct {
	mu sync.Mutex

	nextMsgID int
	entries   []platform.JournalEntry
	messages  []platform.Message // outbound bot messages, WaitForMessage's ordinal addressing

	queue []func(chatID int64)

	submitTextCalls  int
	submitClickCalls int
}

func newFakeChat() *fakeChat { return &fakeChat{} }

// Journal implements observe.Journaler.
func (f *fakeChat) Journal(int64) ([]platform.JournalEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]platform.JournalEntry(nil), f.entries...), nil
}

func (f *fakeChat) reserveMsgIDLocked() int {
	f.nextMsgID++
	return f.nextMsgID
}

// seedBotMessage appends an initial bot message directly, with no preceding
// user message — representing a conversation already in progress when a
// task starts. Returns the message's platform-native ID.
func (f *fakeChat) seedBotMessage(text string, actions [][]platform.Action) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.reserveMsgIDLocked()
	f.appendBotMessageLocked(0, id, text, actions)
	return id
}

// queueBotReply schedules the next SubmitText/SubmitClick call to also
// produce a new bot message.
func (f *fakeChat) queueBotReply(text string, actions [][]platform.Action) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, func(chatID int64) {
		id := f.reserveMsgIDLocked()
		f.appendBotMessageLocked(chatID, id, text, actions)
	})
}

// appendBotMessageLocked appends a bot message to both the journal and the
// WaitForMessage-addressable list. Caller must hold f.mu.
func (f *fakeChat) appendBotMessageLocked(chatID int64, id int, text string, actions [][]platform.Action) {
	f.entries = append(f.entries, platform.JournalEntry{
		Direction: platform.DirectionBot, Kind: platform.JournalEntryMessage,
		MessageID: id, Text: text, Actions: actions, At: time.Now(),
	})
	f.messages = append(f.messages, platform.Message{
		Platform: "fake", ChatID: chatID, MessageID: id, Text: text, Actions: actions, ReceivedAt: time.Now(),
	})
}

// SubmitText implements actor.Actuator.
func (f *fakeChat) SubmitText(chatID int64, _ platform.User, text string) error {
	f.mu.Lock()
	f.submitTextCalls++
	id := f.reserveMsgIDLocked()
	f.entries = append(f.entries, platform.JournalEntry{
		Direction: platform.DirectionUser, Kind: platform.JournalEntryMessage,
		MessageID: id, Text: text, At: time.Now(),
	})
	f.consumeQueueLocked(chatID)
	f.mu.Unlock()
	return nil
}

// SubmitClick implements actor.Actuator.
func (f *fakeChat) SubmitClick(chatID int64, _ platform.User, data string, targetMessageID int) error {
	f.mu.Lock()
	f.submitClickCalls++
	f.entries = append(f.entries, platform.JournalEntry{
		Direction: platform.DirectionUser, Kind: platform.JournalEntryAction,
		RefMessageID: targetMessageID, Text: data, At: time.Now(),
	})
	f.consumeQueueLocked(chatID)
	f.mu.Unlock()
	return nil
}

// consumeQueueLocked runs the next queued bot behaviour, if any. Caller must
// hold f.mu.
func (f *fakeChat) consumeQueueLocked(chatID int64) {
	if len(f.queue) == 0 {
		return
	}
	next := f.queue[0]
	f.queue = f.queue[1:]
	next(chatID)
}

// WaitForMessage implements actor.Actuator. It never actually waits: every
// fakeChat mutation is synchronous, so the (consumed+1)-th message is either
// already there or never coming.
func (f *fakeChat) WaitForMessage(_ int64, consumed int, _ time.Duration) (*platform.Message, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if consumed < 0 || consumed >= len(f.messages) {
		return nil, false
	}
	msg := f.messages[consumed]
	return &msg, true
}

// WaitForEdit implements actor.Actuator. No fakeChat-backed test edits a
// message (only the real-emulator end-to-end test does), so this always
// reports no edit found.
func (f *fakeChat) WaitForEdit(int64, int, int, time.Duration) (*platform.Message, bool) {
	return nil, false
}

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
// Bot behaviour is scripted with queueBotReply/queueBotEdit/queueNoReply,
// consumed FIFO by the next SubmitText or SubmitClick call.
type fakeChat struct {
	mu sync.Mutex

	nextMsgID int
	entries   []platform.JournalEntry
	messages  []platform.Message // outbound bot messages, WaitForMessage's ordinal addressing

	// editVersions tracks the current Version of every bot message this
	// fakeChat has edited in place (queueBotEdit), keyed by MessageID, so
	// WaitForEdit can resolve a real edit the same way a live emulator would
	// — see editBotMessageLocked and WaitForEdit.
	editVersions map[int]int

	queue []func(chatID int64)

	submitTextCalls  int
	submitClickCalls int
}

func newFakeChat() *fakeChat { return &fakeChat{editVersions: make(map[int]int)} }

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

// queueBotEdit schedules the next SubmitText/SubmitClick call to edit an
// existing bot message in place — same MessageID, Version incremented past
// whatever this fakeChat has already recorded for it — rather than sending a
// new message. This is the shape of a bot that re-renders the same logical
// screen in place (greetbot's own callback handler, e.g.), used to exercise
// the loop's semantic no-effect detection when the re-render's text and
// actions are byte-identical to what was already shown (see
// TestNonProgressDetectionSurvivesIdempotentReEdits).
func (f *fakeChat) queueBotEdit(messageID int, text string, actions [][]platform.Action) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, func(chatID int64) {
		f.editBotMessageLocked(chatID, messageID, text, actions)
	})
}

// editBotMessageLocked appends an in-place edit of messageID to the journal
// (Version = one past whatever this fakeChat last recorded for it) and
// updates editVersions so WaitForEdit can resolve it. Unlike
// appendBotMessageLocked, it does NOT add an entry to f.messages: an edit is
// not a new outbound message, so it must not advance WaitForMessage's
// ordinal cursor. Caller must hold f.mu.
func (f *fakeChat) editBotMessageLocked(chatID int64, messageID int, text string, actions [][]platform.Action) {
	version := f.editVersions[messageID] + 1
	f.entries = append(f.entries, platform.JournalEntry{
		Direction: platform.DirectionBot, Kind: platform.JournalEntryMessage,
		MessageID: messageID, Version: version, Text: text, Actions: actions, At: time.Now(),
	})
	f.editVersions[messageID] = version
	_ = chatID // no per-chat state needed beyond the shared journal in this fake
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

// WaitForEdit implements actor.Actuator. It never actually waits (see
// WaitForMessage): if messageID's current Version (as last set by
// queueBotEdit/editBotMessageLocked) is already past afterVersion, its
// latest content is returned immediately; otherwise no edit is found, same
// as the zero-Version default before this fakeChat gained edit support.
func (f *fakeChat) WaitForEdit(chatID int64, messageID int, afterVersion int, _ time.Duration) (*platform.Message, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	version, ok := f.editVersions[messageID]
	if !ok || version <= afterVersion {
		return nil, false
	}
	for i := len(f.entries) - 1; i >= 0; i-- {
		en := f.entries[i]
		if en.Kind != platform.JournalEntryMessage || en.MessageID != messageID || en.Version != version {
			continue
		}
		return &platform.Message{
			Platform: "fake", ChatID: chatID, MessageID: messageID,
			Text: en.Text, Actions: en.Actions, ReceivedAt: en.At, Version: en.Version,
		}, true
	}
	return nil, false
}

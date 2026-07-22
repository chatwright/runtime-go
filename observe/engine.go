package observe

import (
	"fmt"
	"sync"

	"github.com/chatwright/chatwright/platform"
)

// Journaler is the read seam an Engine projects Observations from — the
// subset of platform.Emulator this package depends on. Any platform.Emulator
// satisfies it; tests may supply a narrower fake without running an emulator.
type Journaler interface {
	// Journal returns chatID's chronological, structured journal entries.
	Journal(chatID int64) ([]platform.JournalEntry, error)
}

// Engine projects Observations for one chat from a Journaler's structured
// journal. It owns the per-Engine observation sequence and remembers every
// Observation it has issued, so Changes are always computed by the Engine —
// never by the actor — and a later action proposal can be validated against
// the Observation it was chosen from (see Validate).
type Engine struct {
	journaler Journaler
	chat      ChatRef

	mu   sync.Mutex
	seq  int64
	last *Observation
	// issued remembers every Observation this Engine has produced, by
	// Sequence, so Validate can tell an unknown/fabricated observation
	// sequence apart from a stale one. It grows for the Engine's lifetime;
	// bounding or expiring old entries is left to a future slice (the
	// Observation Model spec itself leaves observation-lifetime open).
	issued map[int64]*Observation
}

// NewEngine constructs an Engine that projects chat's conversation from j.
func NewEngine(j Journaler, chat ChatRef) *Engine {
	return &Engine{journaler: j, chat: chat, issued: make(map[int64]*Observation)}
}

// Observe projects the chat's current journal state into a new Observation.
// Its Changes are computed against the Engine's previously issued
// Observation, if any — the actor is never asked to diff two Observations
// itself.
func (e *Engine) Observe() (*Observation, error) {
	entries, err := e.journaler.Journal(e.chat.ChatID)
	if err != nil {
		return nil, fmt.Errorf("observe: %w", err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.seq++
	obs := &Observation{
		Sequence: e.seq,
		Chat:     e.chat,
		Messages: projectMessages(entries, e.seq),
	}
	if e.last != nil {
		obs.PreviousSequence = e.last.Sequence
		obs.Changes = diff(e.last, obs)
	}
	e.last = obs
	e.issued[obs.Sequence] = obs
	return obs, nil
}

// projectMessages projects entries into the current (latest-version) state of
// each logical message, in first-seen chronological order — a message keeps
// its conversational position across edits, mirroring how a platform's own
// Transcript preserves it (see
// spec/features/chatwright/observation-model/visible-conversation). seq
// stamps every AvailableAction surfaced in the result.
func projectMessages(entries []platform.JournalEntry, seq int64) []VisibleMessage {
	var order []int // journal message IDs in first-seen order
	latest := map[int]platform.JournalEntry{}
	for _, en := range entries {
		if en.Kind != platform.JournalEntryMessage {
			continue
		}
		if _, ok := latest[en.MessageID]; !ok {
			order = append(order, en.MessageID)
		}
		latest[en.MessageID] = en
	}

	messages := make([]VisibleMessage, 0, len(order))
	for _, id := range order {
		en := latest[id]
		messages = append(messages, VisibleMessage{
			ID:      visibleMessageID(en.MessageID),
			Version: en.Version,
			Edited:  en.Version > 0,
			Actor:   actorOf(en.Direction),
			Text:    en.Text,
			Actions: projectActions(en, seq),
		})
	}
	return messages
}

// projectActions converts en's raw platform action rows into the opaque,
// stable AvailableActions an actor sees: a label and a Chatwright-synthetic
// ID derived from the owning message's identity, version and position — never
// the platform's own callback data or button coordinates.
func projectActions(en platform.JournalEntry, seq int64) []AvailableAction {
	var actions []AvailableAction
	for row, cols := range en.Actions {
		for col, a := range cols {
			actions = append(actions, AvailableAction{
				ID:     availableActionID(en.MessageID, en.Version, row, col),
				Label:  a.Label,
				SeenAt: seq,
			})
		}
	}
	return actions
}

func actorOf(d platform.Direction) Actor {
	if d == platform.DirectionBot {
		return ActorBot
	}
	return ActorUser
}

// visibleMessageID derives a VisibleMessage's stable synthetic ID from the
// journal's logical message identity.
func visibleMessageID(journalMessageID int) string { return fmt.Sprintf("msg%d", journalMessageID) }

// availableActionID derives an AvailableAction's stable synthetic ID from the
// owning message's identity, version and position. Encoding version means an
// edit that changes a message's actions (or removes them) mints entirely new
// action IDs, so a proposal targeting the old ones is deterministically stale
// (see Validate) without needing a separate resolver table.
func availableActionID(msgID, version, row, col int) string {
	return fmt.Sprintf("act-%d-%d-r%dc%d", msgID, version, row, col)
}

// diff computes the explicit Changes between prev and curr — new messages,
// edited messages (Version advanced) and actions-changed (Version unchanged,
// available actions differ) — so actors never diff two Observations
// themselves.
func diff(prev, curr *Observation) []Change {
	prevByID := make(map[string]VisibleMessage, len(prev.Messages))
	for _, m := range prev.Messages {
		prevByID[m.ID] = m
	}

	var changes []Change
	for _, m := range curr.Messages {
		prevMsg, existed := prevByID[m.ID]
		switch {
		case !existed:
			changes = append(changes, Change{Kind: ChangeNewMessage, MessageID: m.ID, Actor: m.Actor, Version: m.Version})
		case m.Version > prevMsg.Version:
			changes = append(changes, Change{Kind: ChangeMessageEdited, MessageID: m.ID, Actor: m.Actor, PreviousVersion: prevMsg.Version, Version: m.Version})
		case !sameActionIDs(prevMsg.Actions, m.Actions):
			changes = append(changes, Change{Kind: ChangeActionsChanged, MessageID: m.ID, Actor: m.Actor, Version: m.Version})
		}
	}
	return changes
}

// sameActionIDs reports whether a and b list the same action IDs in the same
// order.
func sameActionIDs(a, b []AvailableAction) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			return false
		}
	}
	return true
}

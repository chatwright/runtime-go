// Package observe implements the minimum slice of Chatwright's Observation
// Model: a platform-neutral projection of a chat's visible conversation and
// available actions, built from a Platform Emulator's structured journal
// (platform.JournalEntry) rather than from any platform's own wire types.
//
// An Engine turns a chat's journal into a sequence of Observations. Each
// Observation carries the chat's currently visible messages, their currently
// available actions, and the explicit Changes since the Engine's previous
// Observation — an actor is never required to diff two Observations by hand
// (see spec/features/chatwright/observation-model/observation-lineage). Raw
// platform payloads (Telegram callback data, native message IDs, wire
// envelopes, ...) never reach this package's exported types: they stay on
// platform.JournalEntry and remain available to developers only through the
// emulator's Transcript/Journal trace, never through an Observation or
// AvailableAction (see
// spec/features/chatwright/observation-model/actor-actions).
//
// This is the minimum working slice — visible messages, generic actions and
// explicit changes. Semantic history windows, summaries, goals and journey
// memory (observation-context) are a later slice; so is the actual
// observe-plan-act-validate actor loop, which is not wired into this package.
package observe

// ChatRef identifies the chat an Observation projects. It carries
// Chatwright's own chat identity (see chatwright.Chat.PrivateChat) — never a
// raw platform chat ID scraped from the wire.
type ChatRef struct {
	ChatID int64 `json:"chatId"`
}

// Actor identifies which side of a conversation produced a VisibleMessage. It
// is a string type, not an int enum, so it marshals to human-readable JSON
// (see AGENTS.md's "JSON artefacts carry human-readable string constants"
// convention) rather than a bare, meaningless integer.
type Actor string

const (
	ActorUser Actor = "user"
	ActorBot  Actor = "bot"
)

// String renders a for diagnostics and test failure messages.
func (a Actor) String() string { return string(a) }

// VisibleMessage is one user-visible logical message: stable identity across
// edits, a monotonic version and an edited flag, plus the actions currently
// attached to it. Only normalized text and action labels are carried — no
// platform-native message IDs, callback data or wire payloads (see
// spec/features/chatwright/observation-model/visible-conversation).
type VisibleMessage struct {
	ID      string            `json:"id"`      // stable synthetic Chatwright identity for this logical message, e.g. "msg7"
	Version int               `json:"version"` // monotonic version of this logical message; 0 for the original send
	Edited  bool              `json:"edited"`  // true once Version has advanced past 0
	Actor   Actor             `json:"actor"`   // who produced the message
	Text    string            `json:"text"`
	Actions []AvailableAction `json:"actions"` // interactions currently attached to this message
}

// AvailableAction is a generic, opaque interaction an actor can take: a
// stable Chatwright ID and its user-visible label. Platform-native callback
// data, request payloads and button coordinates are never exposed here — an
// authorised developer inspector reaches those through the platform's
// Journal/Transcript trace, not through this type (see
// spec/features/chatwright/observation-model/actor-actions).
type AvailableAction struct {
	ID     string `json:"id"`     // opaque, stable Chatwright action identity
	Label  string `json:"label"`  // user-visible text
	SeenAt int64  `json:"seenAt"` // the Observation.Sequence this action was (re)issued at; copy this into an ActionProposal
}

// ChangeKind classifies one entry in an Observation's Changes feed. It is a
// string type, not an int enum, so it marshals to human-readable JSON (see
// AGENTS.md's "JSON artefacts carry human-readable string constants"
// convention) rather than a bare, meaningless integer.
type ChangeKind string

const (
	// ChangeNewMessage: a logical message not present in the Engine's
	// previous Observation now exists.
	ChangeNewMessage ChangeKind = "new-message"
	// ChangeMessageEdited: an existing logical message's Version advanced.
	ChangeMessageEdited ChangeKind = "edited-message"
	// ChangeActionsChanged: an existing logical message's available actions
	// changed without its Version advancing.
	ChangeActionsChanged ChangeKind = "actions-changed"
)

// String renders k for diagnostics and test failure messages.
func (k ChangeKind) String() string { return string(k) }

// Change is one explicit, structured difference between an Observation and
// the Engine's previous Observation, computed by the Engine so actors reason
// about what changed without diffing two Observations themselves (see
// spec/features/chatwright/observation-model/observation-lineage).
type Change struct {
	Kind      ChangeKind `json:"kind"`
	MessageID string     `json:"messageId"`
	Actor     Actor      `json:"actor"`
	// PreviousVersion is set for ChangeMessageEdited: the message's Version
	// before this change.
	PreviousVersion int `json:"previousVersion"`
	// Version is the message's Version after this change (ChangeNewMessage,
	// ChangeMessageEdited) or its current, unchanged Version
	// (ChangeActionsChanged).
	Version int `json:"version"`
}

// Observation is one platform-neutral snapshot of a chat's visible
// conversation and available actions, with explicit lineage back to the
// Engine's previous Observation. Observations are produced by an Engine —
// actors never build or diff one by hand.
type Observation struct {
	// Sequence is monotonic per Engine, starting at 1.
	Sequence int64 `json:"sequence"`
	// PreviousSequence is the Sequence of the Observation this one
	// supersedes; 0 for an Engine's first Observation.
	PreviousSequence int64   `json:"previousSequence"`
	Chat             ChatRef `json:"chat"`
	// Messages is chronological, oldest to newest: one entry per currently
	// visible logical message, at its current (possibly-edited) version.
	Messages []VisibleMessage `json:"messages"`
	// Changes is empty for an Engine's first Observation; otherwise the
	// explicit differences since PreviousSequence.
	Changes []Change `json:"changes"`
}

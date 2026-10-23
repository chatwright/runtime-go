// Package claudechannel implements the Claude Code channel Platform for
// Chatwright: an emulated relay server that lets a real `claude` CLI session
// be driven as the bot-under-test through Anthropic's experimental
// "claude/channel" MCP contract.
//
// Unlike Telegram or WhatsApp, Claude Code has no bot API of its own to
// emulate. Instead, a small relay binary (cmd/chatwright-claudechannel) is
// registered as an MCP stdio server and named on `claude --channels
// server:<name>`. That relay long-polls this package's Emulator over a
// private HTTP protocol and forwards inbound user text to the running Claude
// session as a "notifications/claude/channel" JSON-RPC notification; the
// session replies by calling the relay's "reply" (and optionally
// "edit_message") MCP tool, which the relay posts back to the Emulator. See
// Launch and BuildRelay for starting a real `claude` process against an
// Emulator, and the package-level "Launch mode" and "Org policy and model choice"
// notes below for what was empirically verified.
//
// # Launch mode
//
// `claude --channels server:<name> ...` starts an interactive session that
// requires a TTY (it prints a raw-mode UI even with no prompt argument and
// no piped stdin), so Launch always runs the process under a pseudo-terminal
// (github.com/creack/pty). `claude -p --input-format stream-json --channels
// ...` (print mode) was not pursued as an alternative: getting its stdin
// protocol and required extra flags (`--output-format stream-json
// --verbose`) right added complexity with no clear win, since --channels is
// documented as an interactive-session feature and the pty route worked
// once the gotchas here and the org-policy/model points below were fixed.
// Session.Close sends "/exit" on the pty then kills the whole process
// group, since a plain SIGTERM to the parent alone can leave the
// pty-attached child running.
//
// Gotchas surfaced empirically and handled by Launch:
//
//  1. Setpgid: cmd.SysProcAttr.Setpgid is deliberately NOT set. pty.Start
//     already makes the child a new session leader via setsid (pid == pgid
//     == sid, verified with `ps`), so an explicit Setpgid is both redundant
//     and, in at least one sandboxed environment, rejected with EPERM
//     (a process can't re-parent the group of a session it just created).
//     Close kills -pgid using cmd.Process.Pid directly, which already works
//     off that same invariant.
//  2. First-run workspace trust: Claude Code blocks on a one-time "do you
//     trust this folder?" raw-mode dialog for any directory it has not
//     seen before, which --permission-mode/--dangerously-skip-permissions
//     do not cover (those govern tool-use permission, not this). Driving it
//     with simulated arrow-key + Enter input over the pty was tried and
//     abandoned: Claude Code's ink UI negotiates the Kitty keyboard
//     protocol (`\x1b[>5u` appears in its startup output), under which a
//     naive legacy `\x1b[B\r` (down arrow, Enter) sequence was observed to
//     toggle the menu selection twice instead of confirming it, leaving the
//     dialog open. Launch instead pre-marks opts.WorkDir trusted by writing
//     "hasTrustDialogAccepted": true into its ~/.claude.json entry before
//     starting claude (trustWorkDir) — the same field the real dialog sets
//     — so the dialog never renders.
//  3. CLAUDE*-prefixed environment inheritance: when Launch itself runs
//     inside a claude session (as chatwright's own development did), the
//     child inherits CLAUDECODE, CLAUDE_CODE_CHILD_SESSION and friends from
//     the parent's environment by simple os.Environ() propagation, and
//     Claude Code refuses --channels outright for a session that looks like
//     a nested child ("Channels are not currently available"). Launch
//     strips every "CLAUDE"-prefixed variable before starting the child
//     (sanitizedEnviron); ANTHROPIC_*-prefixed auth vars are untouched.
//  4. Server registration: a manually configured MCP-server channel must be
//     registered as a persistent MCP server (`claude mcp add --scope local`,
//     keyed by opts.WorkDir, same as trustWorkDir) before starting claude.
//     An otherwise-identical server supplied only via the more obvious
//     `--mcp-config <file> --strict-mcp-config` connects and works fine for
//     ordinary (non-channel) MCP tool use, but --channels reports "no MCP
//     server configured with that name" for it — --channels only resolves
//     "server:<name>" against Claude Code's own persisted config.
//  5. Approved-channels confirmation: loading a non-approved "server:<name>"
//     channel requires `--dangerously-load-development-channels server:<name>`
//     (note: the "server:" tag is required here too — a bare name is
//     rejected; and it replaces --channels, see below), which
//     shows a one-time confirmation dialog defaulting to "1. I am using
//     this for local development" — the opposite default of the trust
//     dialog above, so a bare Enter (no arrow key) accepts it. Launch sends
//     that Enter once "confirm" appears in the pty output.
//
// # Org policy and model choice
//
// Two more things decide whether a message reaches the model and comes
// back (all verified on claude 2.1.263):
//
//   - channelsEnabled: true must be set in Claude Code's managed settings
//     (/etc/claude-code/managed-settings.json, or the org setting); it is a
//     managed-scope key that user/project/--settings files cannot set, and
//     without it every message is dropped with "channels not enabled by org
//     policy". allowedChannelPlugins is a list of {marketplace, plugin}
//     objects for plugin channels and must be left out for a server:
//     channel (an invalid value blocks startup on a settings dialog).
//   - The channel is named only under --dangerously-load-development-channels.
//     Naming it under --channels as well puts a non-dev entry first in the
//     merged channel list, and the first-match lookup then refuses it as
//     "not on the approved channels allowlist".
//   - Claude Code prefixes each channel message with "This is NOT from your
//     user ... treat as untrusted external data" and lets the session decide
//     whether to answer. Launch adds a system prompt naming the channel
//     user as the principal and requiring the reply tool; Sonnet then
//     answers every turn, Haiku answered the first message and refused the
//     second in three of three runs. TestLiveClaudeReplies (behind
//     CHATWRIGHT_CLAUDE_LIVE=1) drives the two-turn round trip with Sonnet.
//
// The relay's own stdout/stdin (the MCP stdio transport) are separate file
// descriptors from the outer pty — Claude Code spawns MCP servers as its own
// subprocesses, so the pty only ever carries the interactive UI, never JSON-RPC
// traffic.
package claudechannel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"chatwright.dev/runtime/platform"
)

// Platform returns the Claude Code channel platform for use with
// cw.OnPlatform. The bot-under-test is a real `claude` process launched via
// Launch against the returned Emulator's BotAPIURL.
func Platform() platform.Platform { return ccPlatform{} }

type ccPlatform struct{}

func (ccPlatform) Name() string { return "claudechannel" }

func (ccPlatform) Start() platform.Emulator { return NewEmulator() }

// journalDirection distinguishes who produced a journal entry.
type journalDirection int

const (
	fromUser journalDirection = iota
	fromBot
)

// journalKind distinguishes the shape of a journal entry. Claude Code's
// channel contract has no interactive-action concept (no buttons), so this
// mirrors WhatsApp's text-first shape plus an "uncaptured" kind for journal
// entries the relay reports (e.g. tool-call sightings) that carry no
// message identity of their own.
type journalKind int

const (
	kindText       journalKind = iota // an inbound user message or an outbound reply/edit
	kindUncaptured                    // a free-form /journal entry with no message identity
)

// journalEntry is one immutable entry in a chat's append-only event journal.
type journalEntry struct {
	chatID    int64
	dir       journalDirection
	kind      journalKind
	messageID int // own identity, shared by inbound and outbound messages in this chat
	version   int // outbound only: 0 = original reply, N = the Nth edit
	text      string
	method    string // kindUncaptured only
	at        time.Time
}

// inboxMessage is one user message queued for the relay to deliver to the
// Claude session, as returned by GET /inbox.
type inboxMessage struct {
	ID     int       `json:"id"`
	ChatID int64     `json:"chatID"`
	User   string    `json:"user"`
	Text   string    `json:"text"`
	Ts     time.Time `json:"ts"`
}

// Emulator is an in-process HTTP server standing in for a Claude Code
// "bot API": it queues inbound user messages for the relay to long-poll
// (GET /inbox) and records the relay's outbound reply/edit/journal calls
// (POST /reply, /edit, /journal). It is safe for concurrent use.
type Emulator struct {
	server *httptest.Server

	mu        sync.Mutex
	journal   []journalEntry
	nextMsgID map[int64]int // per-chat shared message-ID sequence, inbound and outbound alike
	updated   chan struct{}

	inbox       []inboxMessage
	inboxWake   chan struct{}
	nextInboxID int
}

// NewEmulator starts a fake Claude Code channel relay server on a random
// local port.
func NewEmulator() *Emulator {
	e := &Emulator{
		nextMsgID: make(map[int64]int),
		updated:   make(chan struct{}),
		inboxWake: make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /inbox", e.handleInbox)
	mux.HandleFunc("POST /reply", e.handleReply)
	mux.HandleFunc("POST /edit", e.handleEdit)
	mux.HandleFunc("POST /journal", e.handleJournal)
	e.server = httptest.NewServer(mux)
	return e
}

// BotAPIURL is the base URL the relay binary must be pointed at (via
// -relay or CHATWRIGHT_RELAY_URL) in place of a real bot API host.
func (e *Emulator) BotAPIURL() string { return e.server.URL }

// Close shuts down the emulator's HTTP server.
func (e *Emulator) Close() { e.server.Close() }

// SetWebhook is a no-op for this platform: the relay pulls work by
// long-polling GET /inbox rather than receiving a pushed webhook, mirroring
// how Telegram's getUpdates polling mode needs no webhook either. url and
// client are accepted only to satisfy platform.Emulator.
func (e *Emulator) SetWebhook(string, *http.Client) {}

// reserveMessageIDLocked returns the next ID in chatID's shared message-ID
// sequence, starting at 1. Caller must hold e.mu.
func (e *Emulator) reserveMessageIDLocked(chatID int64) int {
	e.nextMsgID[chatID]++
	return e.nextMsgID[chatID]
}

// appendLocked appends entry to the journal and wakes any WaitForMessage /
// WaitForEdit waiters. Caller must hold e.mu.
func (e *Emulator) appendLocked(entry journalEntry) {
	e.journal = append(e.journal, entry)
	close(e.updated)
	e.updated = make(chan struct{})
}

// SubmitText enqueues a user's text message for the relay to deliver to the
// Claude session and journals the inbound event. Delivery is pull-based (the
// relay long-polls GET /inbox), so this never fails on a missing webhook the
// way WhatsApp's push delivery does; it always succeeds unless the emulator
// itself is closed.
func (e *Emulator) SubmitText(chatID int64, user platform.User, text string) error {
	e.mu.Lock()
	msgID := e.reserveMessageIDLocked(chatID)
	e.appendLocked(journalEntry{chatID: chatID, dir: fromUser, kind: kindText, messageID: msgID, text: text, at: time.Now()})
	e.nextInboxID++
	e.inbox = append(e.inbox, inboxMessage{
		ID:     e.nextInboxID,
		ChatID: chatID,
		User:   userLabel(user),
		Text:   text,
		Ts:     time.Now(),
	})
	close(e.inboxWake)
	e.inboxWake = make(chan struct{})
	e.mu.Unlock()
	return nil
}

// SubmitClick always fails: the claude/channel MCP contract has no
// interactive-action concept (no buttons, no callback data) — a Claude Code
// session under test can only receive plain text and reply with plain text
// or an edit. Scenarios that need actions should target a platform that
// supports them (Telegram, WhatsApp).
func (e *Emulator) SubmitClick(int64, platform.User, string, int) error {
	return fmt.Errorf("chatwright: claudechannel does not support actions (no buttons in the claude/channel contract)")
}

// userLabel renders a platform.User as the short label the relay attaches to
// a forwarded notification's meta.user field.
func userLabel(u platform.User) string {
	if u.Username != "" {
		return u.Username
	}
	if u.FirstName != "" {
		return u.FirstName
	}
	return fmt.Sprintf("user%d", u.ID)
}

// handleInbox implements GET /inbox?wait=<seconds>: it long-polls for the
// next queued user message and returns it as a single-element JSON array
// (or an empty array on timeout), so the relay's poll loop never has to
// special-case "no message yet" as an error.
func (e *Emulator) handleInbox(w http.ResponseWriter, r *http.Request) {
	waitSeconds := 25.0
	if s := r.URL.Query().Get("wait"); s != "" {
		if v, err := time.ParseDuration(s + "s"); err == nil {
			waitSeconds = v.Seconds()
		}
	}
	deadline := time.After(time.Duration(waitSeconds * float64(time.Second)))

	for {
		e.mu.Lock()
		if len(e.inbox) > 0 {
			msg := e.inbox[0]
			e.inbox = e.inbox[1:]
			e.mu.Unlock()
			writeJSON(w, []inboxMessage{msg})
			return
		}
		wake := e.inboxWake
		e.mu.Unlock()

		select {
		case <-wake:
			continue
		case <-deadline:
			writeJSON(w, []inboxMessage{})
			return
		case <-r.Context().Done():
			return
		}
	}
}

type replyRequest struct {
	ChatID  int64  `json:"chatID"`
	ReplyTo int    `json:"replyTo,omitempty"`
	Text    string `json:"text"`
}

type replyResponse struct {
	MessageID int `json:"messageID"`
}

// handleReply implements POST /reply: it records a new outbound bot message
// (fulfilling any pending WaitForMessage) and returns the assigned message
// ID so the relay can later target it with POST /edit.
func (e *Emulator) handleReply(w http.ResponseWriter, r *http.Request) {
	var req replyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	e.mu.Lock()
	msgID := e.reserveMessageIDLocked(req.ChatID)
	e.appendLocked(journalEntry{chatID: req.ChatID, dir: fromBot, kind: kindText, messageID: msgID, version: 0, text: req.Text, at: time.Now()})
	e.mu.Unlock()
	writeJSON(w, replyResponse{MessageID: msgID})
}

type editRequest struct {
	ChatID    int64  `json:"chatID"`
	MessageID int    `json:"messageID"`
	Text      string `json:"text"`
}

// handleEdit implements POST /edit: it records a new version of an existing
// outbound message, fulfilling any pending WaitForEdit.
func (e *Emulator) handleEdit(w http.ResponseWriter, r *http.Request) {
	var req editRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	e.mu.Lock()
	version := e.nextVersionLocked(req.ChatID, req.MessageID)
	e.appendLocked(journalEntry{chatID: req.ChatID, dir: fromBot, kind: kindText, messageID: req.MessageID, version: version, text: req.Text, at: time.Now()})
	e.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true})
}

// nextVersionLocked returns the next edit version for (chatID, messageID):
// one past the highest version seen so far, or 0 if the message has never
// been recorded (an edit of an unknown message still gets journaled, just
// starting its own version sequence at 0). Caller must hold e.mu.
func (e *Emulator) nextVersionLocked(chatID int64, messageID int) int {
	highest := -1
	for _, en := range e.journal {
		if en.chatID == chatID && en.dir == fromBot && en.kind == kindText && en.messageID == messageID && en.version > highest {
			highest = en.version
		}
	}
	return highest + 1
}

type journalRequest struct {
	ChatID int64  `json:"chatID"`
	Method string `json:"method"`
	Text   string `json:"text,omitempty"`
}

// handleJournal implements POST /journal: a free-form record (e.g. a
// sighting of some other MCP tool call the relay doesn't otherwise
// interpret) appended as a JournalEntryUncaptured entry.
func (e *Emulator) handleJournal(w http.ResponseWriter, r *http.Request) {
	var req journalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	e.mu.Lock()
	e.appendLocked(journalEntry{chatID: req.ChatID, dir: fromBot, kind: kindUncaptured, method: req.Method, text: req.Text, at: time.Now()})
	e.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true})
}

// WaitForMessage waits for the (consumed+1)-th outbound message (original
// sends only, not edits) to chatID.
func (e *Emulator) WaitForMessage(chatID int64, consumed int, timeout time.Duration) (*platform.Message, bool) {
	deadline := time.After(timeout)
	for {
		e.mu.Lock()
		result := e.nthOutboundMessageLocked(chatID, consumed)
		ch := e.updated
		e.mu.Unlock()

		if result != nil {
			return result, true
		}
		select {
		case <-ch:
		case <-deadline:
			return nil, false
		}
	}
}

// nthOutboundMessageLocked returns the (consumed+1)-th distinct bot-sent
// message to chatID (its latest version, if edited), in send order. Caller
// must hold e.mu.
func (e *Emulator) nthOutboundMessageLocked(chatID int64, consumed int) *platform.Message {
	type slot struct {
		firstAt time.Time
		latest  journalEntry
	}
	order := make([]int, 0)
	byID := make(map[int]*slot)
	for _, en := range e.journal {
		if en.chatID != chatID || en.dir != fromBot || en.kind != kindText {
			continue
		}
		s, ok := byID[en.messageID]
		if !ok {
			s = &slot{firstAt: en.at}
			byID[en.messageID] = s
			order = append(order, en.messageID)
		}
		s.latest = en
	}
	if consumed >= len(order) {
		return nil
	}
	en := byID[order[consumed]].latest
	return normalize(&en)
}

// WaitForEdit waits for the message identified by (chatID, messageID) to be
// edited past afterVersion.
func (e *Emulator) WaitForEdit(chatID int64, messageID int, afterVersion int, timeout time.Duration) (*platform.Message, bool) {
	deadline := time.After(timeout)
	for {
		e.mu.Lock()
		result := e.latestEditLocked(chatID, messageID, afterVersion)
		ch := e.updated
		e.mu.Unlock()

		if result != nil {
			return result, true
		}
		select {
		case <-ch:
		case <-deadline:
			return nil, false
		}
	}
}

// latestEditLocked returns the latest recorded version of (chatID,
// messageID) if its version exceeds afterVersion, or nil. Caller must hold
// e.mu.
func (e *Emulator) latestEditLocked(chatID int64, messageID int, afterVersion int) *platform.Message {
	var found *journalEntry
	for i := range e.journal {
		en := &e.journal[i]
		if en.chatID != chatID || en.dir != fromBot || en.kind != kindText || en.messageID != messageID {
			continue
		}
		if found == nil || en.version > found.version {
			found = en
		}
	}
	if found == nil || found.version <= afterVersion {
		return nil
	}
	return normalize(found)
}

// Transcript renders a chronological, human-readable dump of everything
// recorded for chatID.
func (e *Emulator) Transcript(chatID int64) string {
	e.mu.Lock()
	entries := e.journalLocked(chatID)
	e.mu.Unlock()
	return renderTranscript(chatID, entries)
}

// Journal returns chatID's chronological, structured journal entries.
func (e *Emulator) Journal(chatID int64) ([]platform.JournalEntry, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.journalLocked(chatID), nil
}

func (e *Emulator) journalLocked(chatID int64) []platform.JournalEntry {
	entries := make([]platform.JournalEntry, 0, len(e.journal))
	for _, en := range e.journal {
		if en.chatID != chatID {
			continue
		}
		entries = append(entries, toPlatformEntry(en))
	}
	return entries
}

func toPlatformEntry(en journalEntry) platform.JournalEntry {
	pe := platform.JournalEntry{
		MessageID: en.messageID,
		Version:   en.version,
		Text:      en.text,
		Method:    en.method,
		At:        en.at,
	}
	if en.dir == fromBot {
		pe.Direction = platform.DirectionBot
	} else {
		pe.Direction = platform.DirectionUser
	}
	switch en.kind {
	case kindUncaptured:
		pe.Kind = platform.JournalEntryUncaptured
	default:
		pe.Kind = platform.JournalEntryMessage
	}
	return pe
}

func renderTranscript(chatID int64, entries []platform.JournalEntry) string {
	var lines []string
	for _, en := range entries {
		lines = append(lines, renderJournalEntry(en))
	}
	if len(lines) == 0 {
		return fmt.Sprintf("chat %d transcript: (empty — no messages yet)", chatID)
	}
	return fmt.Sprintf("chat %d transcript: %s", chatID, strings.Join(lines, " / "))
}

func renderJournalEntry(en platform.JournalEntry) string {
	if en.Kind == platform.JournalEntryUncaptured {
		return fmt.Sprintf("[uncaptured] %s: %s", en.Method, en.Text)
	}
	who := "user"
	if en.Direction == platform.DirectionBot {
		who = "bot"
	}
	suffix := ""
	if en.Version > 0 {
		suffix = fmt.Sprintf(" (edit v%d)", en.Version)
	}
	return fmt.Sprintf("[%d %s]%s %s", en.MessageID, who, suffix, en.Text)
}

func normalize(en *journalEntry) *platform.Message {
	return &platform.Message{
		Platform:   "claudechannel",
		ChatID:     en.chatID,
		MessageID:  en.messageID,
		Text:       en.text,
		ReceivedAt: en.at,
		Version:    en.version,
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

package claudeprint

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"chatwright.dev/runtime/platform"
)

// maxStderrTail bounds how much of a failed turn's stderr is kept in the
// journal — enough to diagnose a failure, not enough to let one runaway
// process bloat a run bundle.
const maxStderrTail = 4096

// scannerBufSize is the maximum single stream-json line claudeprint will
// parse. The default bufio.Scanner limit (64KiB) is too small for a
// tool_use block carrying a large file read or diff.
const scannerBufSize = 10 << 20 // 10MiB

// ToolCall is one assistant tool_use content block captured from a turn's
// stream-json output.
type ToolCall struct {
	Name    string         // e.g. "Bash", "Read", "Skill"
	Input   map[string]any // the tool_use block's raw "input" object
	Command string         // convenience: Input["command"] when Name == "Bash" and it is a string
}

// TurnMetrics is the accounting stream-json's terminal "result" event
// reports for one turn.
type TurnMetrics struct {
	NumTurns     int
	TotalCostUSD float64
	DurationMS   int64
}

// journalDirection distinguishes who produced a journalEntry.
type journalDirection int

const (
	fromUser journalDirection = iota
	fromBot
)

// journalKind distinguishes the shape of a journalEntry. Print mode has no
// edit or interactive-action concept, so this is text-first plus
// "uncaptured" for tool_use sightings, the stderr tail of a failed turn, and
// per-turn metrics — none of which carry a message identity of their own.
type journalKind int

const (
	kindText       journalKind = iota // an inbound user message or an outbound turn result
	kindUncaptured                    // tool_use / stderr / metrics record
)

// journalEntry is one immutable entry in a chat's append-only event journal.
type journalEntry struct {
	chatID    int64
	dir       journalDirection
	kind      journalKind
	messageID int
	version   int
	text      string
	method    string // kindUncaptured only
	at        time.Time
}

// chatState is the per-chat continuity claudeprint tracks across turns.
type chatState struct {
	sessionID string
	turns     int
}

// Emulator runs one `claude` process per user turn and normalizes its
// stream-json output into Chatwright's neutral Message/Journal model. It
// implements platform.Emulator. Unlike Telegram, WhatsApp or claudechannel,
// it needs no HTTP server of its own: SubmitText spawns the claude process
// directly rather than queuing an update for something else to deliver.
type Emulator struct {
	opts Options

	mu        sync.Mutex
	journal   []journalEntry
	nextMsgID map[int64]int
	updated   chan struct{}
	chats     map[int64]*chatState
	toolCalls map[int64][]ToolCall
	metrics   map[int64][]TurnMetrics
	closed    bool

	wg sync.WaitGroup // in-flight runTurn goroutines
}

// NewEmulator constructs an Emulator that spawns claude processes per opts.
// Most callers should use claudeprint.New instead; NewEmulator is exported
// for tests and callers that build a platform.Emulator directly.
func NewEmulator(opts Options) *Emulator {
	return &Emulator{
		opts:      opts,
		nextMsgID: make(map[int64]int),
		updated:   make(chan struct{}),
		chats:     make(map[int64]*chatState),
		toolCalls: make(map[int64][]ToolCall),
		metrics:   make(map[int64][]TurnMetrics),
	}
}

// BotAPIURL returns "": claudeprint drives claude as a direct subprocess, so
// there is no bot API server for anything to be configured against.
func (e *Emulator) BotAPIURL() string { return "" }

// SetWebhook is a no-op: claudeprint has no update-delivery transport for a
// webhook to receive. Accepted only to satisfy platform.Emulator.
func (e *Emulator) SetWebhook(string, *http.Client) {}

// Close marks the emulator closed (further SubmitText calls fail) and waits
// for any in-flight turn to finish so a test's t.Cleanup does not race a
// still-running claude process past the end of the test.
func (e *Emulator) Close() {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	e.wg.Wait()
}

// reserveMessageIDLocked returns the next ID in chatID's shared message-ID
// sequence, starting at 1. Caller must hold e.mu.
func (e *Emulator) reserveMessageIDLocked(chatID int64) int {
	e.nextMsgID[chatID]++
	return e.nextMsgID[chatID]
}

// appendLocked appends entry to the journal and wakes any WaitForMessage
// waiter. Caller must hold e.mu.
func (e *Emulator) appendLocked(entry journalEntry) {
	e.journal = append(e.journal, entry)
	close(e.updated)
	e.updated = make(chan struct{})
}

// chatStateLocked returns chatID's continuity state, creating it (with a
// freshly generated session ID) on first use. Caller must hold e.mu.
func (e *Emulator) chatStateLocked(chatID int64) *chatState {
	cs, ok := e.chats[chatID]
	if !ok {
		cs = &chatState{sessionID: newSessionID()}
		e.chats[chatID] = cs
	}
	return cs
}

// SubmitText journals the inbound user message, then spawns `claude -p
// <text> ...` in the background for this chat's turn: the first turn of a
// chat passes --session-id <uuid> (freshly generated per chat); every later
// turn passes --resume <uuid> so the conversation continues server-side.
// The returned error reports only that the emulator is closed — normal
// per-turn failures (a non-zero claude exit, a timeout, a missing result
// event) are delivered as an "error: ..." bot message instead, exactly like
// any other bot reply, so WaitForMessage observes them uniformly.
func (e *Emulator) SubmitText(chatID int64, user platform.User, text string) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return fmt.Errorf("chatwright: claudeprint: emulator is closed")
	}
	msgID := e.reserveMessageIDLocked(chatID)
	e.appendLocked(journalEntry{chatID: chatID, dir: fromUser, kind: kindText, messageID: msgID, text: text, at: time.Now()})

	cs := e.chatStateLocked(chatID)
	first := cs.turns == 0
	cs.turns++
	sessionID := cs.sessionID

	e.wg.Add(1)
	e.mu.Unlock()

	go func() {
		defer e.wg.Done()
		e.runTurn(chatID, sessionID, first, text)
	}()
	return nil
}

// SubmitClick always fails: print mode has no interactive-action concept —
// a `claude -p` turn only ever produces text.
func (e *Emulator) SubmitClick(int64, platform.User, string, int) error {
	return fmt.Errorf("chatwright: claudeprint does not support actions (print mode has no buttons)")
}

// buildArgs assembles the claude command line for one turn.
func buildArgs(opts Options, sessionID string, first bool, text string) []string {
	args := []string{"-p", text, "--output-format", "stream-json", "--verbose"}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	args = append(args, "--max-turns", strconv.Itoa(opts.MaxTurns))
	args = append(args, "--allowedTools", opts.AllowedTools)
	for _, dir := range opts.PluginDirs {
		args = append(args, "--plugin-dir", dir)
	}
	if first {
		args = append(args, "--session-id", sessionID)
	} else {
		args = append(args, "--resume", sessionID)
	}
	args = append(args, opts.ExtraArgs...)
	return args
}

// sanitizedEnviron returns os.Environ() with every "CLAUDE*"-prefixed
// variable removed, then opts.Env applied on top. See the package doc's
// "Environment" section for why: a CLAUDE*-prefixed variable leaking from a
// parent claude session into this child would make it misidentify itself
// as a nested session. ANTHROPIC_*-prefixed variables (API auth) are left
// untouched.
func sanitizedEnviron(env map[string]string) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+len(env))
	for _, kv := range base {
		if strings.HasPrefix(kv, "CLAUDE") {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// runTurn spawns claude for one turn, parses its stream-json output, and
// journals the outcome: exactly one outbound bot message (the result text,
// or an "error: ..." message on failure), every tool_use sighting, and the
// turn's metrics.
func (e *Emulator) runTurn(chatID int64, sessionID string, first bool, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), e.opts.TurnTimeout)
	defer cancel()

	args := buildArgs(e.opts, sessionID, first, text)
	cmd := exec.CommandContext(ctx, e.opts.ClaudeBinary, args...)
	cmd.Dir = e.opts.WorkDir
	cmd.Env = sanitizedEnviron(e.opts.Env)

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		e.deliverError(chatID, fmt.Sprintf("open %s: %v", os.DevNull, err), "")
		return
	}
	defer devNull.Close()
	cmd.Stdin = devNull

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		e.deliverError(chatID, fmt.Sprintf("stdout pipe: %v", err), "")
		return
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		e.deliverError(chatID, fmt.Sprintf("start %s: %v", e.opts.ClaudeBinary, err), stderrTail(&stderr))
		return
	}

	var resultText string
	var haveResult bool
	var resultIsError bool
	var turnMetrics TurnMetrics
	var calls []ToolCall

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), scannerBufSize)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		ev, ok := parseStreamEvent(line)
		if !ok {
			continue
		}
		switch ev.Type {
		case "assistant":
			calls = append(calls, toolCallsFrom(ev.Message)...)
		case "result":
			haveResult = true
			resultText = ev.Result
			resultIsError = ev.IsError
			turnMetrics = TurnMetrics{NumTurns: ev.NumTurns, TotalCostUSD: ev.TotalCostUSD, DurationMS: ev.DurationMS}
		}
	}
	scanErr := scanner.Err()

	waitErr := cmd.Wait()

	if len(calls) > 0 {
		e.recordToolCalls(chatID, calls)
	}

	switch {
	case ctx.Err() == context.DeadlineExceeded:
		e.deliverError(chatID, fmt.Sprintf("turn timed out after %s", e.opts.TurnTimeout), stderrTail(&stderr))
	case waitErr != nil:
		e.deliverError(chatID, fmt.Sprintf("claude exited: %v", waitErr), stderrTail(&stderr))
	case scanErr != nil:
		e.deliverError(chatID, fmt.Sprintf("reading claude output: %v", scanErr), stderrTail(&stderr))
	case !haveResult:
		e.deliverError(chatID, "claude produced no result event", stderrTail(&stderr))
	case resultIsError:
		e.deliverError(chatID, resultText, stderrTail(&stderr))
	default:
		e.recordMetrics(chatID, turnMetrics)
		e.deliverMessage(chatID, resultText)
	}
}

// stderrTail returns the last maxStderrTail bytes of buf, so a huge failure
// dump doesn't bloat the journal.
func stderrTail(buf *bytes.Buffer) string {
	s := buf.String()
	if len(s) <= maxStderrTail {
		return s
	}
	return "…(truncated)…" + s[len(s)-maxStderrTail:]
}

// recordToolCalls appends calls to chatID's tool-call log and journals each
// as an uncaptured entry.
func (e *Emulator) recordToolCalls(chatID int64, calls []ToolCall) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.toolCalls[chatID] = append(e.toolCalls[chatID], calls...)
	for _, c := range calls {
		text := c.Command
		if text == "" {
			text = fmt.Sprintf("%v", c.Input)
		}
		e.appendLocked(journalEntry{
			chatID: chatID,
			dir:    fromBot,
			kind:   kindUncaptured,
			method: "tool_use:" + c.Name,
			text:   text,
			at:     time.Now(),
		})
	}
}

// recordMetrics appends m to chatID's metrics log and journals it as an
// uncaptured entry.
func (e *Emulator) recordMetrics(chatID int64, m TurnMetrics) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.metrics[chatID] = append(e.metrics[chatID], m)
	e.appendLocked(journalEntry{
		chatID: chatID,
		dir:    fromBot,
		kind:   kindUncaptured,
		method: "metrics",
		text:   fmt.Sprintf("num_turns=%d total_cost_usd=%f duration_ms=%d", m.NumTurns, m.TotalCostUSD, m.DurationMS),
		at:     time.Now(),
	})
}

// deliverMessage journals text as the turn's single outbound bot message,
// fulfilling any pending WaitForMessage.
func (e *Emulator) deliverMessage(chatID int64, text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	msgID := e.reserveMessageIDLocked(chatID)
	e.appendLocked(journalEntry{chatID: chatID, dir: fromBot, kind: kindText, messageID: msgID, text: text, at: time.Now()})
}

// deliverError journals stderrTail (if any) as an uncaptured entry, then
// delivers "error: <reason>" as the turn's outbound bot message — the same
// delivery path as a successful result, so a scenario asserting on
// ExpectBotMessage sees a failed turn as a message rather than a timeout.
func (e *Emulator) deliverError(chatID int64, reason, stderrText string) {
	e.mu.Lock()
	if stderrText != "" {
		e.appendLocked(journalEntry{chatID: chatID, dir: fromBot, kind: kindUncaptured, method: "stderr", text: stderrText, at: time.Now()})
	}
	msgID := e.reserveMessageIDLocked(chatID)
	e.appendLocked(journalEntry{chatID: chatID, dir: fromBot, kind: kindText, messageID: msgID, text: "error: " + reason, at: time.Now()})
	e.mu.Unlock()
}

// ToolCalls returns every tool_use block recorded for chatID so far, across
// every turn, in the order claude emitted them.
func (e *Emulator) ToolCalls(chatID int64) []ToolCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]ToolCall, len(e.toolCalls[chatID]))
	copy(out, e.toolCalls[chatID])
	return out
}

// Metrics returns the per-turn accounting recorded for chatID so far, one
// entry per completed successful turn, in turn order.
func (e *Emulator) Metrics(chatID int64) []TurnMetrics {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]TurnMetrics, len(e.metrics[chatID]))
	copy(out, e.metrics[chatID])
	return out
}

// WaitForMessage waits for the (consumed+1)-th outbound bot message to
// chatID.
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
// message to chatID, in send order. Caller must hold e.mu.
func (e *Emulator) nthOutboundMessageLocked(chatID int64, consumed int) *platform.Message {
	var order []journalEntry
	for _, en := range e.journal {
		if en.chatID != chatID || en.dir != fromBot || en.kind != kindText {
			continue
		}
		order = append(order, en)
	}
	if consumed >= len(order) {
		return nil
	}
	return normalize(&order[consumed])
}

// WaitForEdit always returns false immediately: print-mode turns never
// produce an in-place edit, so there is nothing to wait for.
func (e *Emulator) WaitForEdit(int64, int, int, time.Duration) (*platform.Message, bool) {
	return nil, false
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
	return fmt.Sprintf("[%d %s] %s", en.MessageID, who, en.Text)
}

func normalize(en *journalEntry) *platform.Message {
	return &platform.Message{
		Platform:   "claudeprint",
		ChatID:     en.chatID,
		MessageID:  en.messageID,
		Text:       en.text,
		ReceivedAt: en.at,
		Version:    en.version,
	}
}

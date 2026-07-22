// Package whatsapp implements the WhatsApp Platform for Chatwright: an emulated
// WhatsApp Cloud API (Graph) server that delivers inbound webhooks and captures
// the bot's outbound calls, normalized to Chatwright's neutral platform types.
//
// This is the MVP-scope, text-first WhatsApp platform. It reuses the
// bots-go-framework WhatsApp adapter github.com/bots-go-framework/bots-api-whatsapp
// (wabotapi) for outbound message decoding. Interactive replies are a planned
// extension; the seam is identical to Telegram's, so a text scenario written once
// runs on either platform.
package whatsapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bots-go-framework/bots-api-whatsapp/wabotapi"

	"github.com/chatwright/chatwright/platform"
)

// Platform returns the WhatsApp platform for use with chatwright.OnPlatform.
func Platform() platform.Platform { return waPlatform{} }

type waPlatform struct{}

func (waPlatform) Name() string { return "whatsapp" }

func (waPlatform) Start() platform.Emulator { return NewEmulator() }

// journalDirection distinguishes who produced a journal entry.
type journalDirection int

const (
	fromUser journalDirection = iota
	fromBot
)

// journalKind distinguishes the shape of a journal entry.
type journalKind int

const (
	kindText     journalKind = iota // an inbound message or an outbound send
	kindCallback                    // an inbound interactive-reply click
)

// journalEntry is one immutable entry in a chat's append-only event journal.
// The WhatsApp Cloud API has no message-edit endpoint, so unlike Telegram's
// journal there is no version concept here: every kindText entry is a
// distinct message.
type journalEntry struct {
	chatID       int64
	dir          journalDirection
	kind         journalKind
	messageID    int // own identity, shared by inbound and outbound messages in this chat; 0 for entries with no message identity of their own (callbacks)
	refMessageID int // kindCallback only: the bot message the click targeted
	text         string
	at           time.Time
}

// Emulator is an in-process HTTP server emulating the WhatsApp Cloud API. It
// owns delivery of updates to the bot-under-test: SubmitText/SubmitClick
// build the inbound webhook payload and push it to the configured webhook —
// the harness never builds wire bytes or POSTs them itself. Unlike Telegram,
// the WhatsApp Cloud API has no polling mode, so a webhook is required.
type Emulator struct {
	server *httptest.Server

	mu        sync.Mutex
	journal   []journalEntry
	nextMsgID map[int64]int // per-chat shared message-ID sequence, inbound and outbound alike
	updated   chan struct{}

	nextEventID int // source for synthetic wamid values that aren't tied to a chat message ID (e.g. interactive-reply clicks)
	webhookURL  string
	httpClient  *http.Client
}

// NewEmulator starts a fake WhatsApp Cloud API server on a random local port.
func NewEmulator() *Emulator {
	e := &Emulator{nextMsgID: make(map[int64]int), updated: make(chan struct{})}
	e.server = httptest.NewServer(http.HandlerFunc(e.handle))
	return e
}

// BotAPIURL is the base URL the bot-under-test should use as its WhatsApp Cloud
// API host, in place of https://graph.facebook.com.
func (e *Emulator) BotAPIURL() string { return e.server.URL }

// Close shuts down the emulator's HTTP server.
func (e *Emulator) Close() { e.server.Close() }

// SetWebhook registers the URL (and HTTP client) the emulator pushes inbound
// webhooks to. The WhatsApp Cloud API has no polling mode: Submit* calls
// return an error until a webhook is set.
func (e *Emulator) SetWebhook(url string, client *http.Client) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.webhookURL = url
	e.httpClient = client
}

// reserveMessageIDLocked returns the next ID in chatID's shared message-ID
// sequence, starting at 1. Caller must hold e.mu.
func (e *Emulator) reserveMessageIDLocked(chatID int64) int {
	e.nextMsgID[chatID]++
	return e.nextMsgID[chatID]
}

// reserveEventIDLocked returns the next synthetic event ID, starting at 1.
// Caller must hold e.mu.
func (e *Emulator) reserveEventIDLocked() int {
	e.nextEventID++
	return e.nextEventID
}

// appendLocked appends entry to the journal and wakes any waiters. Caller must
// hold e.mu.
func (e *Emulator) appendLocked(entry journalEntry) {
	e.journal = append(e.journal, entry)
	close(e.updated)
	e.updated = make(chan struct{})
}

// SubmitText delivers a user's text message to the bot-under-test: it
// reserves the message's ID from chatID's shared sequence, journals the
// inbound event, builds the WhatsApp inbound webhook payload, and pushes it.
// The wabotapi adapter does not yet model inbound text bodies, so the text
// object is built here per the documented WhatsApp Cloud API webhook shape.
func (e *Emulator) SubmitText(chatID int64, user platform.User, text string) error {
	e.mu.Lock()
	msgID := e.reserveMessageIDLocked(chatID)
	e.appendLocked(journalEntry{chatID: chatID, dir: fromUser, kind: kindText, messageID: msgID, text: text, at: time.Now()})
	webhookURL, client := e.webhookURL, e.httpClient
	e.mu.Unlock()

	if webhookURL == "" {
		return fmt.Errorf("no webhook configured; call ServeWebhook or WebhookAt before sending (the WhatsApp Cloud API has no polling mode)")
	}

	waID := strconv.FormatInt(chatID, 10)
	req := inboundRequest{
		Object: "whatsapp_business_account",
		Entry: []inboundEntry{{
			ID: "chatwright",
			Changes: []inboundChange{{
				Field: "messages",
				Value: inboundValue{
					MessagingProduct: "whatsapp",
					Metadata:         wabotapi.WebhookMetadata{DisplayPhoneNumber: "15550000000", PhoneNumberID: "chatwright-phone"},
					Contacts: []wabotapi.WebhookContact{{
						Profile: wabotapi.WebhookContactProfile{Name: user.FirstName},
						WaID:    waID,
					}},
					Messages: []inboundMessage{{
						From:      waID,
						ID:        "wamid." + strconv.Itoa(msgID),
						Timestamp: strconv.FormatInt(time.Now().Unix(), 10),
						Type:      string(wabotapi.InboundMessageTypeText),
						Text:      &inboundText{Body: text},
					}},
				},
			}},
		}},
	}
	return e.post(webhookURL, client, req)
}

// SubmitClick delivers a user's interactive-reply click to the bot-under-test.
// It does not reserve a message ID: the click references an existing message
// (targetMessageID) rather than creating a new one.
func (e *Emulator) SubmitClick(chatID int64, user platform.User, data string, targetMessageID int) error {
	e.mu.Lock()
	e.appendLocked(journalEntry{chatID: chatID, dir: fromUser, kind: kindCallback, refMessageID: targetMessageID, text: data, at: time.Now()})
	eventID := e.reserveEventIDLocked()
	webhookURL, client := e.webhookURL, e.httpClient
	e.mu.Unlock()

	if webhookURL == "" {
		return fmt.Errorf("no webhook configured; call ServeWebhook or WebhookAt before clicking (the WhatsApp Cloud API has no polling mode)")
	}

	waID := strconv.FormatInt(chatID, 10)
	req := inboundRequest{
		Object: "whatsapp_business_account",
		Entry: []inboundEntry{{
			ID: "chatwright",
			Changes: []inboundChange{{
				Field: "messages",
				Value: inboundValue{
					MessagingProduct: "whatsapp",
					Metadata:         wabotapi.WebhookMetadata{DisplayPhoneNumber: "15550000000", PhoneNumberID: "chatwright-phone"},
					Contacts: []wabotapi.WebhookContact{{
						Profile: wabotapi.WebhookContactProfile{Name: user.FirstName},
						WaID:    waID,
					}},
					Messages: []inboundMessage{{
						From:      waID,
						ID:        "wamid." + strconv.Itoa(eventID),
						Timestamp: strconv.FormatInt(time.Now().Unix(), 10),
						Type:      string(wabotapi.InboundMessageTypeInteractive),
						Interactive: &inboundInteractive{
							Type:        "button_reply",
							ButtonReply: &inboundButtonReply{ID: data, Title: data},
						},
					}},
				},
			}},
		}},
	}
	return e.post(webhookURL, client, req)
}

// post JSON-encodes req and POSTs it to webhookURL, blocking until the
// bot-under-test's handler returns — exactly like a real webhook push, which
// is why every SendText/Click call in a scenario can assume the bot has
// already processed it by the time the call returns.
func (e *Emulator) post(webhookURL string, client *http.Client, req inboundRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("chatwright: encode update: %w", err)
	}
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Post(webhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("chatwright: deliver update to webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("chatwright: webhook returned status %d", resp.StatusCode)
	}
	return nil
}

// handle emulates POST /{version}/{phoneNumberID}/messages.
func (e *Emulator) handle(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(strings.TrimSuffix(r.URL.Path, "/"), "/messages") {
		writeJSON(w, map[string]any{"success": true})
		return
	}

	var cfg wabotapi.SendTextConfig
	_ = json.NewDecoder(r.Body).Decode(&cfg)
	chatID, _ := strconv.ParseInt(cfg.To, 10, 64)

	e.mu.Lock()
	messageID := e.reserveMessageIDLocked(chatID)
	e.appendLocked(journalEntry{chatID: chatID, dir: fromBot, kind: kindText, messageID: messageID, text: cfg.Text.Body, at: time.Now()})
	e.mu.Unlock()

	// Mirror the WhatsApp Cloud API success envelope.
	writeJSON(w, map[string]any{
		"messaging_product": "whatsapp",
		"contacts":          []map[string]string{{"wa_id": cfg.To}},
		"messages":          []map[string]string{{"id": "wamid.reply"}},
	})
}

// WaitForMessage waits for the (consumed+1)-th outbound message to chatID.
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
	seen := 0
	for _, en := range e.journal {
		if en.chatID != chatID || en.dir != fromBot || en.kind != kindText {
			continue
		}
		if seen == consumed {
			return normalize(&en)
		}
		seen++
	}
	return nil
}

// WaitForEdit always reports no edit: the WhatsApp Cloud API has no
// message-edit endpoint, so a sent text message can never change in place.
func (e *Emulator) WaitForEdit(int64, int, int, time.Duration) (*platform.Message, bool) {
	return nil, false
}

// Transcript renders a chronological, human-readable dump of everything
// recorded for chatID — inbound user messages, outbound bot messages and
// interactive-reply clicks — for inclusion in assertion failure messages.
func (e *Emulator) Transcript(chatID int64) string {
	e.mu.Lock()
	defer e.mu.Unlock()

	var lines []string
	for _, en := range e.journal {
		if en.chatID != chatID {
			continue
		}
		lines = append(lines, renderEntry(en))
	}
	if len(lines) == 0 {
		return fmt.Sprintf("chat %d transcript: (empty — no messages yet)", chatID)
	}
	return fmt.Sprintf("chat %d transcript: %s", chatID, strings.Join(lines, " / "))
}

// renderEntry renders one transcript line for en.
func renderEntry(en journalEntry) string {
	if en.kind == kindCallback {
		return fmt.Sprintf("[user] clicked %q on message %d", en.text, en.refMessageID)
	}
	who := "user"
	if en.dir == fromBot {
		who = "bot"
	}
	return fmt.Sprintf("[%d %s] %s", en.messageID, who, en.text)
}

// normalize converts a journal entry into a neutral message.
func normalize(en *journalEntry) *platform.Message {
	return &platform.Message{
		Platform:   "whatsapp",
		ChatID:     en.chatID,
		MessageID:  en.messageID,
		Text:       en.text,
		ReceivedAt: en.at,
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// Inbound webhook shape. Reuses wabotapi types where available; adds the inbound
// text object the adapter does not yet model.
type inboundRequest struct {
	Object string         `json:"object"`
	Entry  []inboundEntry `json:"entry"`
}

type inboundEntry struct {
	ID      string          `json:"id"`
	Changes []inboundChange `json:"changes"`
}

type inboundChange struct {
	Field string       `json:"field"`
	Value inboundValue `json:"value"`
}

type inboundValue struct {
	MessagingProduct string                    `json:"messaging_product"`
	Metadata         wabotapi.WebhookMetadata  `json:"metadata"`
	Contacts         []wabotapi.WebhookContact `json:"contacts,omitempty"`
	Messages         []inboundMessage          `json:"messages,omitempty"`
}

type inboundMessage struct {
	From        string              `json:"from"`
	ID          string              `json:"id"`
	Timestamp   string              `json:"timestamp"`
	Type        string              `json:"type"`
	Text        *inboundText        `json:"text,omitempty"`
	Interactive *inboundInteractive `json:"interactive,omitempty"`
}

type inboundText struct {
	Body string `json:"body"`
}

type inboundInteractive struct {
	Type        string              `json:"type"`
	ButtonReply *inboundButtonReply `json:"button_reply,omitempty"`
}

type inboundButtonReply struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

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
	"encoding/json"
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

type outgoing struct {
	chatID     int64
	messageID  int
	text       string
	receivedAt time.Time
}

// Emulator is an in-process HTTP server emulating the WhatsApp Cloud API.
type Emulator struct {
	server *httptest.Server

	mu            sync.Mutex
	calls         []*outgoing
	nextMessageID int
	updated       chan struct{}
}

// NewEmulator starts a fake WhatsApp Cloud API server on a random local port.
func NewEmulator() *Emulator {
	e := &Emulator{nextMessageID: 1, updated: make(chan struct{})}
	e.server = httptest.NewServer(http.HandlerFunc(e.handle))
	return e
}

// BotAPIURL is the base URL the bot-under-test should use as its WhatsApp Cloud
// API host, in place of https://graph.facebook.com.
func (e *Emulator) BotAPIURL() string { return e.server.URL }

// Close shuts down the emulator's HTTP server.
func (e *Emulator) Close() { e.server.Close() }

// EncodeInboundText builds a WhatsApp inbound webhook carrying a user's text.
// The wabotapi adapter does not yet model inbound text bodies, so the text object
// is added here per the documented WhatsApp Cloud API webhook shape.
func (e *Emulator) EncodeInboundText(in platform.Inbound) (string, []byte) {
	waID := strconv.FormatInt(in.ChatID, 10)
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
						Profile: wabotapi.WebhookContactProfile{Name: in.User.FirstName},
						WaID:    waID,
					}},
					Messages: []inboundMessage{{
						From:      waID,
						ID:        "wamid." + strconv.Itoa(in.MessageID),
						Timestamp: strconv.FormatInt(time.Now().Unix(), 10),
						Type:      string(wabotapi.InboundMessageTypeText),
						Text:      &inboundText{Body: in.Text},
					}},
				},
			}},
		}},
	}
	body, _ := json.Marshal(req)
	return "application/json", body
}

// EncodeCallback builds an inbound WhatsApp interactive button reply (a click).
func (e *Emulator) EncodeCallback(in platform.InboundCallback) (string, []byte) {
	waID := strconv.FormatInt(in.ChatID, 10)
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
						Profile: wabotapi.WebhookContactProfile{Name: in.User.FirstName},
						WaID:    waID,
					}},
					Messages: []inboundMessage{{
						From:      waID,
						ID:        "wamid." + strconv.Itoa(in.UpdateID),
						Timestamp: strconv.FormatInt(time.Now().Unix(), 10),
						Type:      string(wabotapi.InboundMessageTypeInteractive),
						Interactive: &inboundInteractive{
							Type:        "button_reply",
							ButtonReply: &inboundButtonReply{ID: in.Data, Title: in.Data},
						},
					}},
				},
			}},
		}},
	}
	body, _ := json.Marshal(req)
	return "application/json", body
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
	messageID := e.nextMessageID
	e.nextMessageID++
	e.calls = append(e.calls, &outgoing{chatID: chatID, messageID: messageID, text: cfg.Text.Body, receivedAt: time.Now()})
	close(e.updated)
	e.updated = make(chan struct{})
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
		var match *outgoing
		seen := 0
		for _, c := range e.calls {
			if c.chatID == chatID {
				if seen == consumed {
					match = c
					break
				}
				seen++
			}
		}
		ch := e.updated
		e.mu.Unlock()

		if match != nil {
			return &platform.Message{
				Platform:   "whatsapp",
				ChatID:     match.chatID,
				MessageID:  match.messageID,
				Text:       match.text,
				ReceivedAt: match.receivedAt,
			}, true
		}
		select {
		case <-ch:
		case <-deadline:
			return nil, false
		}
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

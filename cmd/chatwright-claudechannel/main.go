// Command chatwright-claudechannel is the MCP stdio relay Claude Code loads
// via `claude --channels server:<name> --mcp-config <file>`. It implements
// Anthropic's experimental "claude/channel" contract: it long-polls a
// chatwright.dev/runtime/claudechannel Emulator's GET /inbox for user
// messages and forwards each as a "notifications/claude/channel" JSON-RPC
// notification; the Claude session answers by calling the "reply" (and
// optionally "edit_message") tool, which this relay posts back to the
// emulator's POST /reply / POST /edit.
//
// stdout carries only newline-delimited JSON-RPC 2.0 messages (the MCP stdio
// transport); all logging goes to stderr.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

func main() {
	relayURL := flag.String("relay", os.Getenv("CHATWRIGHT_RELAY_URL"), "base URL of the chatwright claudechannel Emulator")
	instructions := flag.String("instructions", "", "override the MCP server instructions text sent at initialize")
	flag.Parse()

	if *relayURL == "" {
		fmt.Fprintln(os.Stderr, "chatwright-claudechannel: -relay (or CHATWRIGHT_RELAY_URL) is required")
		os.Exit(2)
	}

	logger := log.New(os.Stderr, "chatwright-claudechannel: ", log.LstdFlags|log.Lmsgprefix)
	s := newServer(*relayURL, *instructions, logger)
	if err := s.run(os.Stdin, os.Stdout); err != nil && err != io.EOF {
		logger.Printf("exiting: %v", err)
		os.Exit(1)
	}
}

// server holds the relay's runtime state: the HTTP client talking to the
// Emulator and the stdio JSON-RPC writer talking to the Claude session.
type server struct {
	relayURL     string
	instructions string
	logger       *log.Logger
	client       *http.Client

	out   io.Writer
	outMu sync.Mutex // guards concurrent writes to out (tool responses vs. async poll notifications)

	initialized bool
	pollStop    chan struct{}
}

func newServer(relayURL, instructions string, logger *log.Logger) *server {
	if instructions == "" {
		instructions = "This channel connects to a Chatwright test harness, not a real chat client. " +
			"Inbound user messages arrive as notifications/claude/channel notifications carrying " +
			"meta.chat_id, meta.message_id, meta.user and meta.ts. The user can only see what you " +
			"send with the reply tool (pass the same chat_id); text you write in the terminal is " +
			"never delivered to them. Answer every channel message by calling reply exactly once. " +
			"Use edit_message to revise a message you already sent."
	}
	return &server{
		relayURL:     relayURL,
		instructions: instructions,
		logger:       logger,
		client:       &http.Client{Timeout: 35 * time.Second},
	}
}

// jsonrpcRequest is the subset of JSON-RPC 2.0 request fields this relay
// needs. ID is json.RawMessage so a missing ID (a notification) round-trips
// as nil without forcing every request to be number-typed.
type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type jsonrpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// run reads newline-delimited JSON-RPC requests from in and writes responses
// (and asynchronous notifications, once the session is initialized) to out.
// It returns when in is closed (the Claude session shut down the relay), the
// standard clean-shutdown signal for an MCP stdio server.
func (s *server) run(in io.Reader, out io.Writer) error {
	s.out = out
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req jsonrpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.logger.Printf("malformed request: %v", err)
			continue
		}
		s.handle(req)
	}
	s.stopPolling()
	return scanner.Err()
}

// handle dispatches one JSON-RPC request/notification.
func (s *server) handle(req jsonrpcRequest) {
	switch req.Method {
	case "initialize":
		s.writeResult(req.ID, map[string]any{
			"protocolVersion": "2025-06-18",
			"serverInfo":      map[string]any{"name": "chatwright-claudechannel", "version": "0.1.0"},
			"capabilities": map[string]any{
				"tools":        map[string]any{},
				"experimental": map[string]any{"claude/channel": map[string]any{}},
			},
			"instructions": s.instructions,
		})
	case "notifications/initialized":
		s.initialized = true
		s.startPolling()
	case "ping":
		s.writeResult(req.ID, map[string]any{})
	case "tools/list":
		s.writeResult(req.ID, map[string]any{"tools": toolDefs})
	case "tools/call":
		s.handleToolCall(req)
	default:
		if req.ID != nil {
			s.writeError(req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
		}
	}
}

var toolDefs = []map[string]any{
	{
		"name":        "reply",
		"description": "Send a message to the user on this channel. This is the only way the user sees your answer: terminal output is not delivered. Call it once per inbound channel message, with that message's meta.chat_id.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"chat_id":  map[string]any{"type": "string"},
				"text":     map[string]any{"type": "string"},
				"reply_to": map[string]any{"type": "string"},
			},
			"required": []string{"chat_id", "text"},
		},
	},
	{
		"name":        "edit_message",
		"description": "Edit a message previously sent with reply.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message_id": map[string]any{"type": "string"},
				"text":       map[string]any{"type": "string"},
			},
			"required": []string{"message_id", "text"},
		},
	},
}

type toolCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func (s *server) handleToolCall(req jsonrpcRequest) {
	var params toolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.writeError(req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	switch params.Name {
	case "reply":
		s.callReply(req.ID, params.Arguments)
	case "edit_message":
		s.callEditMessage(req.ID, params.Arguments)
	default:
		s.writeToolError(req.ID, fmt.Sprintf("unknown tool: %s", params.Name))
	}
}

func (s *server) callReply(id json.RawMessage, args map[string]any) {
	chatID, _ := args["chat_id"].(string)
	text, _ := args["text"].(string)
	replyTo, _ := args["reply_to"].(string)
	if chatID == "" || text == "" {
		s.writeToolError(id, "reply requires chat_id and text")
		return
	}

	body := map[string]any{"chatID": parseChatID(chatID), "text": text}
	if replyTo != "" {
		body["replyTo"] = replyTo
	}
	var resp struct {
		MessageID int `json:"messageID"`
	}
	if err := s.postJSON("/reply", body, &resp); err != nil {
		s.writeToolError(id, err.Error())
		return
	}
	s.writeResult(id, map[string]any{
		"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("sent (%d)", resp.MessageID)}},
	})
}

func (s *server) callEditMessage(id json.RawMessage, args map[string]any) {
	messageID, _ := args["message_id"].(string)
	text, _ := args["text"].(string)
	if messageID == "" || text == "" {
		s.writeToolError(id, "edit_message requires message_id and text")
		return
	}
	body := map[string]any{"messageID": parseChatID(messageID), "text": text}
	if err := s.postJSON("/edit", body, nil); err != nil {
		s.writeToolError(id, err.Error())
		return
	}
	s.writeResult(id, map[string]any{
		"content": []map[string]any{{"type": "text", "text": "ok"}},
	})
}

// parseChatID parses a decimal chat/message ID string, defaulting to 0 on a
// malformed value rather than failing the whole tool call outright — the
// relay's job is to move messages, not to validate scenario input.
func parseChatID(s string) int64 {
	var n int64
	_, _ = fmt.Sscanf(s, "%d", &n)
	return n
}

// startPolling begins the background loop that long-polls the emulator's
// GET /inbox and forwards each message as a notification. It is idempotent:
// only the first "notifications/initialized" starts it.
func (s *server) startPolling() {
	if s.pollStop != nil {
		return
	}
	s.pollStop = make(chan struct{})
	go s.pollLoop(s.pollStop)
}

func (s *server) stopPolling() {
	if s.pollStop != nil {
		close(s.pollStop)
		s.pollStop = nil
	}
}

type inboxMessage struct {
	ID     int       `json:"id"`
	ChatID int64     `json:"chatID"`
	User   string    `json:"user"`
	Text   string    `json:"text"`
	Ts     time.Time `json:"ts"`
}

func (s *server) pollLoop(stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		var msgs []inboxMessage
		if err := s.getJSON("/inbox?wait=25", &msgs); err != nil {
			s.logger.Printf("inbox poll: %v", err)
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
			}
			continue
		}
		for _, m := range msgs {
			s.notifyMessage(m)
		}
	}
}

func (s *server) notifyMessage(m inboxMessage) {
	s.writeNotification("notifications/claude/channel", map[string]any{
		"content": m.Text,
		"meta": map[string]any{
			"chat_id":    fmt.Sprintf("%d", m.ChatID),
			"message_id": fmt.Sprintf("%d", m.ID),
			"user":       m.User,
			"ts":         m.Ts.Format(time.RFC3339),
		},
	})
}

func (s *server) postJSON(path string, body, out any) error {
	return s.doJSON(http.MethodPost, path, body, out)
}

func (s *server) getJSON(path string, out any) error {
	return s.doJSON(http.MethodGet, path, nil, out)
}

func (s *server) doJSON(method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = jsonReader(b)
	}
	req, err := http.NewRequest(method, s.relayURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("relay %s %s: status %d", method, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func jsonReader(b []byte) io.Reader { return &byteReader{b: b} }

type byteReader struct{ b []byte }

func (r *byteReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

// writeResult, writeError and writeNotification serialize one JSON-RPC
// message per line to stdout, synchronized against the async poll loop's
// notifications so lines never interleave.
func (s *server) writeResult(id json.RawMessage, result any) {
	s.writeLine(jsonrpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *server) writeError(id json.RawMessage, code int, message string) {
	s.writeLine(jsonrpcResponse{JSONRPC: "2.0", ID: id, Error: &jsonrpcError{Code: code, Message: message}})
}

// writeToolError reports a tool-call failure the MCP way: a normal result
// carrying isError:true, not a JSON-RPC protocol error — the tool ran, it
// just failed.
func (s *server) writeToolError(id json.RawMessage, message string) {
	s.writeLine(jsonrpcResponse{JSONRPC: "2.0", ID: id, Result: map[string]any{
		"content": []map[string]any{{"type": "text", "text": message}},
		"isError": true,
	}})
}

func (s *server) writeNotification(method string, params any) {
	s.writeLine(jsonrpcNotification{JSONRPC: "2.0", Method: method, Params: params})
}

func (s *server) writeLine(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		s.logger.Printf("encode: %v", err)
		return
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	_, _ = s.out.Write(b)
	_, _ = s.out.Write([]byte("\n"))
}

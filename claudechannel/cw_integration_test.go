package claudechannel_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"chatwright.dev/runtime/claudechannel"
	"chatwright.dev/runtime/cw"
)

// fakeClaude polls an emulator's /inbox and replies with a canned response,
// standing in for a real `claude --channels ...` session so the neutral cw
// API can be exercised offline (no ANTHROPIC credentials, no cost).
type fakeClaude struct {
	client  *http.Client
	baseURL string
}

func startFakeClaude(ctx context.Context, baseURL string) {
	fc := &fakeClaude{client: http.DefaultClient, baseURL: baseURL}
	go fc.loop(ctx)
}

func (fc *fakeClaude) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fc.baseURL+"/inbox?wait=1", nil)
		resp, err := fc.client.Do(req)
		if err != nil {
			return
		}
		var msgs []struct {
			ChatID int64  `json:"chatID"`
			Text   string `json:"text"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&msgs)
		_ = resp.Body.Close()
		for _, m := range msgs {
			reply := map[string]any{"chatID": m.ChatID, "text": "echo: " + m.Text}
			b, _ := json.Marshal(reply)
			preq, _ := http.NewRequestWithContext(ctx, http.MethodPost, fc.baseURL+"/reply", bytes.NewReader(b))
			preq.Header.Set("Content-Type", "application/json")
			r, err := fc.client.Do(preq)
			if err == nil {
				_ = r.Body.Close()
			}
		}
	}
}

// TestCwAPI_DrivesClaudeChannelPlatform proves cw.New(t,
// cw.OnPlatform(claudechannel.Platform())) works with the neutral SendText /
// ExpectBotMessage verbs unchanged, using an in-process fake "claude" so the
// test runs offline with no real claude process or API usage.
func TestCwAPI_DrivesClaudeChannelPlatform(t *testing.T) {
	w := cw.New(t, cw.OnPlatform(claudechannel.Platform()))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startFakeClaude(ctx, w.BotAPIURL())

	chat := w.PrivateChat(cw.User{ID: "alice", FirstName: "Alice"})
	chat.SendText("hi")
	chat.ExpectBotMessage().Within(2 * time.Second).Text("echo: hi")
}

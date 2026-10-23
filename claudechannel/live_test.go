package claudechannel_test

import (
	"context"
	"os"
	"testing"
	"time"

	"chatwright.dev/runtime/claudechannel"
	"chatwright.dev/runtime/cw"
)

// TestLiveClaudeReplies drives a real `claude` process (haiku, minimal
// turns) as the bot-under-test over the claude/channel MCP contract. It is
// gated behind CHATWRIGHT_CLAUDE_LIVE=1 because it costs real usage against
// an authenticated subscription; it is not part of the default test run.
//
// It proves two things in one session: a single turn round-trips, and a
// second turn in the SAME session still works (multi-turn), which is the
// whole point of driving Claude Code as a long-running channel rather than
// one-shot `claude -p`.
//
// It needs channelsEnabled: true in Claude Code's managed settings and a
// model that follows the relay's instructions (Sonnet by default, see
// liveModel); the package doc's "Org policy and model choice" section has
// the details. Session.Output() is logged on cleanup for diagnosis.
func TestLiveClaudeReplies(t *testing.T) {
	if os.Getenv("CHATWRIGHT_CLAUDE_LIVE") != "1" {
		t.Skip("set CHATWRIGHT_CLAUDE_LIVE=1 to run this test against a real claude process (costs usage)")
	}

	workDir := t.TempDir()
	relayBin := claudechannel.BuildRelay(t)

	w := cw.New(t, cw.OnPlatform(claudechannel.Platform()))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)

	sess, err := claudechannel.Launch(ctx, claudechannel.LaunchOptions{
		RelayBinary: relayBin,
		RelayURL:    w.BotAPIURL(),
		WorkDir:     workDir,
		Model:       liveModel(),
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() {
		t.Logf("session pty output:\n%s", sess.Output())
		if err := sess.Close(); err != nil {
			t.Logf("Session.Close: %v", err)
		}
	})

	chat := w.PrivateChat(cw.User{ID: "alice", FirstName: "Alice"})

	chat.SendText("Reply with exactly the word PONG and nothing else.")
	chat.ExpectBotMessage().Within(120 * time.Second).TextContains("PONG")

	chat.SendText("Now reply with exactly the word PING.")
	chat.ExpectBotMessage().Within(120 * time.Second).TextContains("PING")
}

// liveModel is the model the live test drives; CHATWRIGHT_CLAUDE_MODEL
// overrides the default. Haiku answers the first channel message but tends to
// treat later ones as "untrusted external data" (Claude Code's own channel
// preamble says so) and stays silent; Sonnet follows the relay's
// instructions reliably.
func liveModel() string {
	if m := os.Getenv("CHATWRIGHT_CLAUDE_MODEL"); m != "" {
		return m
	}
	return "sonnet"
}

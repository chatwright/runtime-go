package claudeprint_test

import (
	"os"
	"testing"
	"time"

	"chatwright.dev/runtime/claudeprint"
	"chatwright.dev/runtime/cw"
)

// TestLiveClaudePrintReplies drives a real `claude -p` process (haiku, one
// process per turn) as the bot-under-test. It is gated behind
// CHATWRIGHT_CLAUDE_LIVE=1 because it costs real usage against an
// authenticated subscription; it is not part of the default test run.
//
// It proves two things in one chat: a single print-mode turn round-trips a
// result, and a second turn against the same chat still sees the first
// turn's context via --resume <session-id> — the whole point of tracking a
// session ID per chat rather than treating every turn as independent.
func TestLiveClaudePrintReplies(t *testing.T) {
	if os.Getenv("CHATWRIGHT_CLAUDE_LIVE") != "1" {
		t.Skip("set CHATWRIGHT_CLAUDE_LIVE=1 to run this test against a real claude process (costs usage)")
	}

	workDir := "/tmp/claude-1000/-home-ai-projects/acb421b9-b1dd-49fc-8468-de7dc60f1392/scratchpad/claudeprint-live"
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workDir: %v", err)
	}

	w := cw.New(t, cw.OnPlatform(claudeprint.New(
		claudeprint.WithModel("haiku"),
		claudeprint.WithWorkDir(workDir),
		claudeprint.WithTurnTimeout(2*time.Minute),
	)))

	chat := w.PrivateChat(cw.User{ID: "alice", FirstName: "Alice"})

	chat.SendText("The secret word is pineapple. Reply OK.")
	chat.ExpectBotMessage().Within(120 * time.Second).IsTextMessage()

	chat.SendText("What is the secret word? One word.")
	chat.ExpectBotMessage().Within(120 * time.Second).TextMatches("(?i)pineapple")
}

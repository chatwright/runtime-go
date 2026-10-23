package claudeprint_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"chatwright.dev/runtime/claudeprint"
	"chatwright.dev/runtime/cw"
)

// TestCwAPI_DrivesClaudeprintPlatform proves cw.New(t,
// cw.OnPlatform(claudeprint.New(...))) works with the neutral SendText /
// ExpectBotMessage verbs unchanged, using a fake "claude" binary so the
// test runs offline with no real claude process or API usage.
func TestCwAPI_DrivesClaudeprintPlatform(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/usr/bin/env bash\nset -u\n" +
		`prompt=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-p" ]; then prompt="$arg"; fi
  prev="$arg"
done
echo "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"echo: $prompt\",\"num_turns\":1,\"total_cost_usd\":0.005,\"duration_ms\":3}"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}

	w := cw.New(t, cw.OnPlatform(claudeprint.New(
		claudeprint.WithClaudeBinary(path),
		claudeprint.WithWorkDir(t.TempDir()),
		claudeprint.WithTurnTimeout(10*time.Second),
	)))

	chat := w.PrivateChat(cw.User{ID: "alice", FirstName: "Alice"})
	chat.SendText("hi")
	chat.ExpectBotMessage().Within(2 * time.Second).Text("echo: hi")
}

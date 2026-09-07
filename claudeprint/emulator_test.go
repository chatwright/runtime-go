package claudeprint_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chatwright.dev/runtime/claudeprint"
	"chatwright.dev/runtime/platform"
)

// writeFakeClaude writes script (a bash script body, without the shebang
// line) to an executable file in t.TempDir() and returns its path. It
// stands in for a real `claude` binary in offline tests, exercising the
// exact stream-json protocol Emulator.runTurn parses without any network
// call or ANTHROPIC credentials.
func writeFakeClaude(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude")
	content := "#!/usr/bin/env bash\nset -u\n" + script + "\n"
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return path
}

const alice = 42

func aliceUser() platform.User { return platform.User{ID: alice, FirstName: "Alice"} }

// TestSubmitText_FirstTurnSessionID_SecondTurnResume proves the first
// SubmitText for a chat passes --session-id <uuid> and the second passes
// --resume with that same uuid, and that both turns' result text is
// delivered as a single bot message each.
func TestSubmitText_FirstTurnSessionID_SecondTurnResume(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state")
	script := `
prompt=""
session=""
mode=""
prev=""
for arg in "$@"; do
  case "$prev" in
    -p) prompt="$arg" ;;
    --session-id) session="$arg"; mode="new" ;;
    --resume) session="$arg"; mode="resume" ;;
  esac
  prev="$arg"
done

if [ "$mode" = "new" ]; then
  echo -n "$session" > "` + stateFile + `"
  result="turn1:$prompt"
else
  stored="$(cat "` + stateFile + `" 2>/dev/null || true)"
  if [ "$stored" != "$session" ]; then
    echo "session mismatch: stored=$stored got=$session" >&2
    exit 1
  fi
  result="turn2-resumed:$prompt"
fi

echo "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"session_id\":\"$session\",\"result\":\"$result\",\"num_turns\":1,\"total_cost_usd\":0.01,\"duration_ms\":42}"
`
	bin := writeFakeClaude(t, script)
	e := claudeprint.NewEmulator(claudeprint.Options{ClaudeBinary: bin, TurnTimeout: 10 * time.Second})
	t.Cleanup(e.Close)

	if err := e.SubmitText(alice, aliceUser(), "hello"); err != nil {
		t.Fatalf("SubmitText #1: %v", err)
	}
	msg1, ok := e.WaitForMessage(alice, 0, 5*time.Second)
	if !ok {
		t.Fatalf("no message for turn 1\n%s", e.Transcript(alice))
	}
	if msg1.Text != "turn1:hello" {
		t.Fatalf("turn 1 text = %q, want %q", msg1.Text, "turn1:hello")
	}

	if err := e.SubmitText(alice, aliceUser(), "again"); err != nil {
		t.Fatalf("SubmitText #2: %v", err)
	}
	msg2, ok := e.WaitForMessage(alice, 1, 5*time.Second)
	if !ok {
		t.Fatalf("no message for turn 2\n%s", e.Transcript(alice))
	}
	if msg2.Text != "turn2-resumed:again" {
		t.Fatalf("turn 2 text = %q, want %q (proves --resume reused the same session id)", msg2.Text, "turn2-resumed:again")
	}
}

// TestToolCallsRecorded proves an assistant tool_use block lands in both
// the Journal (as an uncaptured entry) and the typed ToolCalls accessor,
// with the Bash "command" field surfaced on ToolCall.Command.
func TestToolCallsRecorded(t *testing.T) {
	script := `
session=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "--session-id" ] || [ "$prev" = "--resume" ]; then session="$arg"; fi
  prev="$arg"
done
echo "{\"type\":\"assistant\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"working\"},{\"type\":\"tool_use\",\"id\":\"tu1\",\"name\":\"Bash\",\"input\":{\"command\":\"sneat action commit\"}}]}}"
echo "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"session_id\":\"$session\",\"result\":\"done\",\"num_turns\":1,\"total_cost_usd\":0.02,\"duration_ms\":7}"
`
	bin := writeFakeClaude(t, script)
	e := claudeprint.NewEmulator(claudeprint.Options{ClaudeBinary: bin, TurnTimeout: 10 * time.Second})
	t.Cleanup(e.Close)

	if err := e.SubmitText(alice, aliceUser(), "commit please"); err != nil {
		t.Fatalf("SubmitText: %v", err)
	}
	if _, ok := e.WaitForMessage(alice, 0, 5*time.Second); !ok {
		t.Fatalf("no message\n%s", e.Transcript(alice))
	}

	calls := e.ToolCalls(alice)
	if len(calls) != 1 {
		t.Fatalf("ToolCalls = %d entries, want 1: %+v", len(calls), calls)
	}
	if calls[0].Name != "Bash" {
		t.Errorf("ToolCalls[0].Name = %q, want %q", calls[0].Name, "Bash")
	}
	if calls[0].Command != "sneat action commit" {
		t.Errorf("ToolCalls[0].Command = %q, want %q", calls[0].Command, "sneat action commit")
	}

	journal, err := e.Journal(alice)
	if err != nil {
		t.Fatalf("Journal: %v", err)
	}
	var sawToolUse bool
	for _, en := range journal {
		if en.Kind == platform.JournalEntryUncaptured && en.Method == "tool_use:Bash" {
			sawToolUse = true
			if !strings.Contains(en.Text, "sneat action commit") {
				t.Errorf("tool_use journal entry text = %q, want it to contain the command", en.Text)
			}
		}
	}
	if !sawToolUse {
		t.Errorf("Journal has no tool_use:Bash entry: %+v", journal)
	}

	metrics := e.Metrics(alice)
	if len(metrics) != 1 || metrics[0].TotalCostUSD != 0.02 {
		t.Errorf("Metrics = %+v, want one entry with TotalCostUSD 0.02", metrics)
	}
}

// TestCLAUDEEnvStripped proves every CLAUDE*-prefixed environment variable
// is removed from the spawned claude process's environment before
// Options.Env is applied, so a claudeprint test suite running inside a
// claude session itself never leaks CLAUDECODE et al. into the child.
func TestCLAUDEEnvStripped(t *testing.T) {
	t.Setenv("CLAUDE_LEAK_TEST", "should-not-be-visible")

	script := `
echo "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"CLAUDE_LEAK_TEST=[${CLAUDE_LEAK_TEST:-}]\",\"num_turns\":1,\"total_cost_usd\":0,\"duration_ms\":1}"
`
	bin := writeFakeClaude(t, script)
	e := claudeprint.NewEmulator(claudeprint.Options{ClaudeBinary: bin, TurnTimeout: 10 * time.Second})
	t.Cleanup(e.Close)

	if err := e.SubmitText(alice, aliceUser(), "hi"); err != nil {
		t.Fatalf("SubmitText: %v", err)
	}
	msg, ok := e.WaitForMessage(alice, 0, 5*time.Second)
	if !ok {
		t.Fatalf("no message\n%s", e.Transcript(alice))
	}
	if msg.Text != "CLAUDE_LEAK_TEST=[]" {
		t.Fatalf("child process saw CLAUDE_LEAK_TEST leak through: %q", msg.Text)
	}
}

// TestNonZeroExitBecomesErrorMessage proves a claude process that exits
// non-zero without ever emitting a "result" event is delivered as a single
// "error: ..." bot message (never a scenario timeout), with its stderr tail
// recorded in the journal.
func TestNonZeroExitBecomesErrorMessage(t *testing.T) {
	script := `
echo "kaboom: something went wrong" >&2
exit 3
`
	bin := writeFakeClaude(t, script)
	e := claudeprint.NewEmulator(claudeprint.Options{ClaudeBinary: bin, TurnTimeout: 10 * time.Second})
	t.Cleanup(e.Close)

	if err := e.SubmitText(alice, aliceUser(), "hi"); err != nil {
		t.Fatalf("SubmitText: %v", err)
	}
	msg, ok := e.WaitForMessage(alice, 0, 5*time.Second)
	if !ok {
		t.Fatalf("no message\n%s", e.Transcript(alice))
	}
	if !strings.HasPrefix(msg.Text, "error:") {
		t.Fatalf("message text = %q, want it to start with %q", msg.Text, "error:")
	}

	journal, err := e.Journal(alice)
	if err != nil {
		t.Fatalf("Journal: %v", err)
	}
	var sawStderr bool
	for _, en := range journal {
		if en.Kind == platform.JournalEntryUncaptured && en.Method == "stderr" {
			sawStderr = true
			if !strings.Contains(en.Text, "kaboom") {
				t.Errorf("stderr journal entry = %q, want it to contain %q", en.Text, "kaboom")
			}
		}
	}
	if !sawStderr {
		t.Errorf("Journal has no stderr entry: %+v", journal)
	}
}

// TestSubmitClickUnsupported proves SubmitClick always errors — print mode
// has no buttons.
func TestSubmitClickUnsupported(t *testing.T) {
	e := claudeprint.NewEmulator(claudeprint.Options{ClaudeBinary: "unused"})
	t.Cleanup(e.Close)
	if err := e.SubmitClick(alice, aliceUser(), "data", 1); err == nil {
		t.Fatal("SubmitClick: want an error, got nil")
	}
}

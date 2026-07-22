// Package pybot_test drives pybot.py — a Telegram bot written in nothing but
// the Python standard library — as a real subprocess through Chatwright,
// proving the "bot-under-test can be written in any language" claim from the
// README with a runnable example rather than a documentation assertion.
package pybot_test

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/chatwright/chatwright"
)

// readyTimeout bounds how long the test waits for the Python process to
// start accepting connections on its webhook port before giving up.
const readyTimeout = 5 * time.Second

// freeAddr reserves a free local TCP address by binding to port 0, reading
// back what the OS assigned, then releasing it. Both the emulator (via
// chatwright.WithListenAddr) and pybot (via the PORT env var) need their
// address decided up front, before either side is started.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a free address: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release reserved address: %v", err)
	}
	return addr
}

// startPybot launches examples/pybot/pybot.py as a real subprocess,
// configured entirely through the two environment variables it documents:
// TELEGRAM_API_ROOT (the emulator's Bot API base URL) and PORT (the local
// port its webhook listens on). Both addresses are decided by the caller
// before this returns, which is exactly the seam chatwright.WithListenAddr
// exists for: an externally-started, non-Go process needs its API base URL
// in its environment before it starts, not read back from the harness
// afterwards.
//
// It waits for the process to actually accept connections before returning,
// runs it in its own process group so cleanup can kill the whole group (a
// misbehaving interpreter spawning children would otherwise leak them), and
// registers that cleanup with t so no pybot process outlives the test —
// including on failure, and safely under -race.
func startPybot(t *testing.T, apiRoot, botAddr string) {
	t.Helper()

	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found on PATH: skipping the non-Go pybot example (this test proves the README's any-language claim against a real Python process, so it has nothing to fall back to)")
	}

	_, botPort, err := net.SplitHostPort(botAddr)
	if err != nil {
		t.Fatalf("split bot address %q: %v", botAddr, err)
	}

	scriptPath, err := filepath.Abs("pybot.py")
	if err != nil {
		t.Fatalf("resolve pybot.py path: %v", err)
	}

	cmd := exec.Command(pythonPath, scriptPath)
	cmd.Env = append(os.Environ(),
		"TELEGRAM_API_ROOT="+apiRoot,
		"PORT="+botPort,
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	// New process group so cleanup can kill pybot and any child it spawns
	// together, rather than leaving orphans behind if the interpreter forks.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start pybot: %v", err)
	}

	t.Cleanup(func() {
		if pid := cmd.Process.Pid; pid > 0 {
			// Negative pid signals the whole process group started above.
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		_ = cmd.Wait() // reap; the process was killed, so its own exit error is expected and uninteresting
		if t.Failed() {
			t.Logf("pybot output:\n%s", output.String())
		}
	})

	waitForListener(t, botAddr)
}

// waitForListener polls addr until something accepts a TCP connection there,
// or fails the test after readyTimeout — without this, the first webhook
// delivery could race pybot's own HTTPServer construction.
func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(readyTimeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pybot never started listening on %s within %s", addr, readyTimeout)
}

// TestPybotEndToEnd drives pybot.py through Chatwright exactly like any
// other bot-under-test: send a text and expect a greeting, then /start and
// click its one inline button and expect the message to be edited in place —
// the same shape as examples/greetbot, proving it doesn't matter that this
// bot is a separate Python process rather than in-process Go.
func TestPybotEndToEnd(t *testing.T) {
	apiAddr := freeAddr(t)
	botAddr := freeAddr(t)

	// The API base URL is fully known here, before Chatwright itself has
	// booted — this is what lets it be handed to pybot's environment ahead
	// of time, matching how a real external bot process is configured.
	apiRoot := "http://" + apiAddr
	startPybot(t, apiRoot, botAddr)

	cw := chatwright.New(t, chatwright.WithListenAddr(apiAddr))
	if got := cw.BotAPIURL(); got != apiRoot {
		t.Fatalf("BotAPIURL() = %q, want %q", got, apiRoot)
	}
	cw.WebhookAt("http://" + botAddr + "/webhook")

	chat := cw.PrivateChat(chatwright.User{ID: "alice", FirstName: "Alice"})

	chat.SendText("Hi")
	chat.ExpectBotMessage().
		Within(2 * time.Second).
		Text("Howdy stranger")

	chat.SendText("/start")
	msg := chat.ExpectBotMessage().
		Within(2 * time.Second).
		IsTextMessage()
	msg.Text("Choose an action")
	msg.ExpectAction(0, 0).
		Label("Click me").
		ID("clicked").
		Click()

	msg.ExpectEdited().
		Within(2 * time.Second).
		Text("You clicked it!")
}

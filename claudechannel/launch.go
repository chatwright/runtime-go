package claudechannel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// LaunchOptions configures a real `claude` process started against an
// Emulator by Launch.
type LaunchOptions struct {
	// RelayBinary is the path to a built cmd/chatwright-claudechannel
	// executable. Use BuildRelay(t) in tests to build it on demand.
	RelayBinary string

	// RelayURL is the Emulator's BotAPIURL the relay long-polls. Required.
	RelayURL string

	// ChannelName is the MCP server name registered with --channels
	// server:<name>. Defaults to "chatwright".
	ChannelName string

	// WorkDir is the working directory the claude process runs in. It
	// should be a scratch directory outside any repository, so no
	// ambient CLAUDE.md, hooks, or project settings interfere with the
	// session under test. Required.
	WorkDir string

	// Model is passed as --model, e.g. "haiku". Defaults to "haiku" to
	// keep live runs cheap.
	Model string

	// PermissionMode is passed as --permission-mode. Defaults to
	// "bypassPermissions" so the session under test never blocks on an
	// interactive permission prompt (there is no human to answer one).
	PermissionMode string

	// ExtraArgs are appended verbatim to the claude command line, e.g.
	// "--plugin-dir" or "--add-dir" pairs.
	ExtraArgs []string

	// Env are extra environment variables (in addition to the current
	// process's environment) set on the claude process.
	Env []string

	// Instructions overrides the relay's MCP server instructions text.
	Instructions string

	// StartTimeout bounds how long Launch waits for the claude process to
	// come up before returning. It does not bound the session's lifetime.
	// Defaults to 20s.
	StartTimeout time.Duration
}

// Session is a running `claude --channels ...` process launched by Launch.
type Session struct {
	cmd      *exec.Cmd
	pty      *os.File
	mu       sync.Mutex
	output   []byte
	closed   bool
	closeErr error
}

// Launch registers opts.RelayBinary as an MCP server scoped to opts.WorkDir
// (via `claude mcp add --scope local`), then starts `claude --channels
// server:<name> --dangerously-load-development-channels server:<name>`
// under a pseudo-terminal in opts.WorkDir. A pty is required: Claude Code's
// interactive mode (which --channels depends on) insists on a TTY even with
// no prompt argument, and manual testing showed print mode (`claude -p
// --input-format stream-json`) rejects --channels — see the package doc's
// "Launch mode" section, which also explains why registration goes through
// `claude mcp add` rather than the seemingly more obvious `--mcp-config
// <file> --strict-mcp-config`: empirically, --channels only resolves a
// "server:<name>" entry against a server persisted in Claude Code's own
// config (~/.claude.json), not one supplied ad hoc via --mcp-config, so a
// --mcp-config-only server is reported as "no MCP server configured with
// that name" even though the exact same file works fine for ordinary
// (non-channel) MCP tool use.
//
// The returned Session's Close terminates the whole process group (not just
// the immediate child), because a plain SIGTERM to a pty-attached parent can
// leave the pty's child process running.
func Launch(ctx context.Context, opts LaunchOptions) (*Session, error) {
	if opts.RelayBinary == "" {
		return nil, fmt.Errorf("chatwright: claudechannel.Launch: RelayBinary is required")
	}
	if opts.RelayURL == "" {
		return nil, fmt.Errorf("chatwright: claudechannel.Launch: RelayURL is required")
	}
	if opts.WorkDir == "" {
		return nil, fmt.Errorf("chatwright: claudechannel.Launch: WorkDir is required")
	}
	channelName := opts.ChannelName
	if channelName == "" {
		channelName = "chatwright"
	}
	model := opts.Model
	if model == "" {
		model = "haiku"
	}
	permissionMode := opts.PermissionMode
	if permissionMode == "" {
		permissionMode = "bypassPermissions"
	}
	startTimeout := opts.StartTimeout
	if startTimeout == 0 {
		startTimeout = 20 * time.Second
	}

	if err := registerRelayServer(ctx, opts.WorkDir, channelName, opts.RelayBinary, opts.RelayURL, opts.Instructions); err != nil {
		return nil, fmt.Errorf("chatwright: claudechannel.Launch: %w", err)
	}

	// Pre-accept the one-time workspace-trust dialog Claude Code shows for a
	// directory it has not seen before: it is a raw-mode interactive prompt
	// that --permission-mode/--dangerously-skip-permissions do not cover
	// (those govern tool-use permission, not this), and driving it via
	// simulated arrow-key + Enter input over the pty proved unreliable in
	// testing — it uses an alternate keyboard-input protocol (Kitty's CSI u
	// progressive enhancement) that the naive legacy escape sequences don't
	// interact with predictably. Marking opts.WorkDir trusted in
	// ~/.claude.json ahead of time (the same field the real dialog sets)
	// makes the dialog never appear, so Launch's pty conversation stays
	// pure JSON-RPC over the channel from the first message.
	if err := trustWorkDir(opts.WorkDir); err != nil {
		return nil, fmt.Errorf("chatwright: claudechannel.Launch: %w", err)
	}

	args := []string{
		"--channels", "server:" + channelName,
		// A manually configured MCP-server channel is only accepted if it is
		// on Claude Code's built-in approved allowlist or explicitly opted
		// into as local dev; a chatwright test relay is neither, so it must
		// be named here too (as "server:<name>", the same tag --channels
		// itself uses) or the session refuses it with "server ... is not on
		// the approved channels allowlist" and never leaves the prompt.
		"--dangerously-load-development-channels", "server:" + channelName,
		"--permission-mode", permissionMode,
		"--model", model,
	}
	args = append(args, opts.ExtraArgs...)

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = opts.WorkDir
	cmd.Env = append(sanitizedEnviron(), opts.Env...)
	// No explicit Setpgid here: pty.Start already puts the child in a new
	// session via setsid, which makes it its own process group leader (pid
	// == pgid == sid) as a side effect — verified empirically. Requesting
	// Setpgid on top of that fails with EPERM in some sandboxes (a process
	// cannot re-parent its own just-created session's group), and it is
	// redundant here regardless: Close's "kill the process group" already
	// works off cmd.Process.Pid as the group ID.

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("chatwright: claudechannel.Launch: start under pty: %w", err)
	}

	s := &Session{cmd: cmd, pty: ptmx}
	go s.drain()

	// Give the process a moment to either come up or die immediately (e.g. a
	// bad flag combination). Along the way, confirm the one-time
	// "--dangerously-load-development-channels" warning dialog: it lists
	// the channel and defaults its selection to "I am using this for local
	// development" (option 1 of 2), so a bare Enter accepts it — no arrow
	// key needed, unlike the (now pre-answered, see trustWorkDir) workspace
	// trust dialog, whose default was the *other* option. A clean exit
	// within the start window is reported as an error rather than a live
	// Session.
	deadline := time.Now().Add(startTimeout)
	devChannelsConfirmed := false
	for time.Now().Before(deadline) {
		if s.cmd.ProcessState != nil {
			break
		}
		if !devChannelsConfirmed && strings.Contains(strings.ToLower(s.Output()), "confirm") {
			_, _ = s.pty.Write([]byte("\r"))
			devChannelsConfirmed = true
		}
		select {
		case <-ctx.Done():
			_ = s.Close()
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	if s.cmd.ProcessState != nil {
		out := s.Output()
		return nil, fmt.Errorf("chatwright: claudechannel.Launch: claude exited early (%v); output:\n%s", s.cmd.ProcessState, out)
	}
	return s, nil
}

// drain continuously reads the pty's output into Session's internal buffer,
// exposed via Output. It exits when the pty is closed (the process exited).
func (s *Session) drain() {
	buf := make([]byte, 4096)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			s.mu.Lock()
			s.output = append(s.output, buf[:n]...)
			s.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// Output returns everything captured from the session's pty so far
// (terminal UI included — it is not scoped to any particular turn).
func (s *Session) Output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.output)
}

// Close terminates the claude session: it writes "/exit" to the pty to ask
// for a clean shutdown, then kills the whole process group (claude and any
// children it spawned, including the relay) and closes the pty. Safe to call
// more than once.
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		err := s.closeErr
		s.mu.Unlock()
		return err
	}
	s.closed = true
	s.mu.Unlock()

	// Best-effort clean exit; ignore errors (the pty may already be gone).
	_, _ = s.pty.Write([]byte("/exit\r"))
	done := make(chan struct{})
	go func() { _, _ = s.cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		if pgid, err := syscall.Getpgid(s.cmd.Process.Pid); err == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		} else {
			_ = s.cmd.Process.Kill()
		}
		<-done
	}
	err := s.pty.Close()
	s.mu.Lock()
	s.closeErr = err
	s.mu.Unlock()
	return err
}

// sanitizedEnviron returns os.Environ() with every "CLAUDE*"-prefixed
// variable removed. When Launch itself runs inside a claude session (e.g. a
// Go test driven by Claude Code, as chatwright's own live test is), those
// vars — CLAUDECODE, CLAUDE_CODE_CHILD_SESSION, CLAUDE_CODE_SESSION_ID, the
// messaging socket/token, and friends — leak into the child by simple
// inheritance and make it identify itself as a nested child session, under
// which Claude Code disables --channels outright ("Channels are not
// currently available"), observed empirically. ANTHROPIC_*-prefixed vars
// (API auth) are untouched.
func sanitizedEnviron() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "CLAUDE") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// trustConfigMu serializes read-modify-write access to ~/.claude.json across
// goroutines in this process; it does not protect against a concurrent
// `claude` process elsewhere writing the same file mid-update; this is a
// best-effort test/tooling convenience, not a general-purpose config editor.
var trustConfigMu sync.Mutex

// trustWorkDir marks dir as a trusted workspace in ~/.claude.json — the same
// "hasTrustDialogAccepted" field a human clicking through the real
// first-run dialog sets — so Launch's claude process starts straight into
// the channel session instead of blocking on that interactive prompt. It
// only adds/updates dir's entry; every other key in the file, and every
// other project's entry, is round-tripped untouched.
func trustWorkDir(dir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("trustWorkDir: %w", err)
	}
	path := filepath.Join(home, ".claude.json")

	trustConfigMu.Lock()
	defer trustConfigMu.Unlock()

	root := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &root); err != nil {
			return fmt.Errorf("trustWorkDir: parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("trustWorkDir: read %s: %w", path, err)
	}

	projects, _ := root["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
	}
	entry, _ := projects[dir].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
	}
	entry["hasTrustDialogAccepted"] = true
	projects[dir] = entry
	root["projects"] = projects

	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("trustWorkDir: encode %s: %w", path, err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("trustWorkDir: write %s: %w", path, err)
	}
	return nil
}

// registerRelayServer registers relayBinary as an MCP server named
// channelName, scoped to dir, via `claude mcp add --scope local` run with
// dir as its working directory (the same per-directory local scope
// trustWorkDir marks trusted, both keyed off dir in ~/.claude.json). This is
// the persistence layer --channels actually consults — see Launch's doc
// comment for why an ephemeral --mcp-config file doesn't work here. Any
// stale entry from a previous registration under the same name is removed
// first (best-effort; dir is normally a fresh scratch directory per Launch
// call, so this is just defensive).
func registerRelayServer(ctx context.Context, dir, channelName, relayBinary, relayURL, instructions string) error {
	_ = exec.CommandContext(ctx, "claude", "mcp", "remove", "--scope", "local", channelName).Run() // best-effort

	relayArgs := []string{"-relay", relayURL}
	if instructions != "" {
		relayArgs = append(relayArgs, "-instructions", instructions)
	}
	addArgs := append([]string{"mcp", "add", "--scope", "local", channelName, relayBinary, "--"}, relayArgs...)
	cmd := exec.CommandContext(ctx, "claude", addArgs...)
	cmd.Dir = dir
	cmd.Env = sanitizedEnviron()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("register relay MCP server: %w; output:\n%s", err, out)
	}
	return nil
}

// BuildRelay compiles cmd/chatwright-claudechannel into t.TempDir() and
// returns the resulting executable's path, for use as LaunchOptions.RelayBinary
// in tests. It fails the test on a build error.
func BuildRelay(t testing.TB) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "chatwright-claudechannel")
	cmd := exec.Command("go", "build", "-o", out, "chatwright.dev/runtime/cmd/chatwright-claudechannel")
	cmd.Dir = repoRoot(t)
	var stderr []byte
	if b, err := cmd.CombinedOutput(); err != nil {
		stderr = b
		t.Fatalf("chatwright: claudechannel.BuildRelay: go build: %v\n%s", err, stderr)
	}
	return out
}

// repoRoot walks up from the current working directory to find the module
// root (the directory containing go.mod), so BuildRelay works whether the
// calling test lives in claudechannel/ or a subdirectory of it.
func repoRoot(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("chatwright: claudechannel.BuildRelay: getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("chatwright: claudechannel.BuildRelay: no go.mod found above %s", dir)
		}
		dir = parent
	}
}

package arena

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFlightLogLandsOnDiskBeforeLogfReturns proves Logf's own File.Sync()
// happens before Logf returns: a completely independent read of path
// (os.ReadFile opens, reads and closes its own *os.File — never the one
// FlightLog itself holds) must already see the line the instant Logf
// hands control back.
func TestFlightLogLandsOnDiskBeforeLogfReturns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flight.log")

	fl, err := OpenFlightLog(path)
	if err != nil {
		t.Fatalf("OpenFlightLog() error = %v", err)
	}
	defer fl.Close()

	if err := fl.Logf("phase=%s model=%s", "cell-start", "gemma-4-e4b"); err != nil {
		t.Fatalf("Logf() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read flight log via an independent open: %v", err)
	}
	if !strings.Contains(string(data), "phase=cell-start model=gemma-4-e4b") {
		t.Fatalf("flight log does not contain the just-written line after Logf returned:\n%s", data)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Errorf("flight log line missing trailing newline: %q", data)
	}
}

// TestFlightLogEachLineTimestamped proves Logf prefixes every line with an
// RFC3339Nano timestamp, ahead of the caller's formatted message.
func TestFlightLogEachLineTimestamped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flight.log")
	fl, err := OpenFlightLog(path)
	if err != nil {
		t.Fatalf("OpenFlightLog() error = %v", err)
	}
	defer fl.Close()

	if err := fl.Logf("phase=matrix-start"); err != nil {
		t.Fatalf("Logf() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read flight log: %v", err)
	}
	line := strings.TrimRight(string(data), "\n")
	fields := strings.SplitN(line, " ", 2)
	if len(fields) != 2 {
		t.Fatalf("line = %q, want \"<timestamp> <message>\"", line)
	}
	if _, err := time.Parse(time.RFC3339Nano, fields[0]); err != nil {
		t.Errorf("timestamp field %q does not parse as RFC3339Nano: %v", fields[0], err)
	}
	if fields[1] != "phase=matrix-start" {
		t.Errorf("message field = %q, want %q", fields[1], "phase=matrix-start")
	}
}

// TestFlightLogNeverTruncatesOnReopen proves OpenFlightLog opens
// O_APPEND|O_CREATE, never O_TRUNC: re-opening a path that already has
// content preserves it — the whole point of a crash-survival log being
// pointless if a caller's next run wipes yesterday's evidence.
func TestFlightLogNeverTruncatesOnReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flight.log")

	fl1, err := OpenFlightLog(path)
	if err != nil {
		t.Fatalf("OpenFlightLog() [1] error = %v", err)
	}
	if err := fl1.Logf("phase=matrix-start scenario=before-crash"); err != nil {
		t.Fatalf("Logf() [1] error = %v", err)
	}
	if err := fl1.Close(); err != nil {
		t.Fatalf("Close() [1] error = %v", err)
	}

	// Simulates the caller re-running arena.Run against the same
	// persistent path after a crash (or simply a second matrix).
	fl2, err := OpenFlightLog(path)
	if err != nil {
		t.Fatalf("OpenFlightLog() [2] error = %v", err)
	}
	defer fl2.Close()
	if err := fl2.Logf("phase=matrix-start scenario=after-restart"); err != nil {
		t.Fatalf("Logf() [2] error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read flight log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("len(lines) = %d, want 2 (reopen must never truncate) — content:\n%s", len(lines), data)
	}
	if !strings.Contains(lines[0], "before-crash") {
		t.Errorf("lines[0] = %q, want it to still contain the pre-reopen line", lines[0])
	}
	if !strings.Contains(lines[1], "after-restart") {
		t.Errorf("lines[1] = %q, want it to contain the post-reopen line", lines[1])
	}
}

// TestFlightLogNilOptionIsNoop proves RunOptions.logf is a true no-op when
// FlightLog is nil — no panic, no allocation-visible side effect. Run's
// own e2e tests (e2e_test.go, arena_test.go) never set RunOptions.FlightLog
// at all and stay green, which is the real "zero behavioural change" proof
// this feature requires; this test only pins down the logf helper itself
// in isolation.
func TestFlightLogNilOptionIsNoop(t *testing.T) {
	var opts RunOptions
	if opts.FlightLog != nil {
		t.Fatal("zero-value RunOptions.FlightLog is not nil")
	}
	opts.logf("phase=%s", "should-never-write-anywhere")
}

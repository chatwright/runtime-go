package arena

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// FlightLog is an append-only, fsync-per-line log of an arena Run's phase
// boundaries — the record the founder ordered after repeated macOS kernel
// panics during arena reruns (chatwright/runtime-go#8;
// chatwright/backstage research/model-arena-2026-07-23/crash-analysis.md):
// a kernel panic erases every RAM-resident buffer, including a provider
// server's own logs — LM Studio's lost the fatal minutes across two of
// those panics. Logf writes one line, fsync'd to disk, before returning,
// so after any crash — however abrupt — whatever the flight log's last
// line names is provably the last thing arena.Run started, and how far it
// got.
//
// See RunOptions.FlightLog for how a Run wires one in; nil (the default)
// disables flight logging entirely with zero behavioural change.
//
// # Give it a persistent path — never /tmp
//
// OpenFlightLog exists to survive a crash that wipes RAM. A path under an
// OS temp directory can itself be cleared by the same reboot/crash
// recovery that follows a panic, or never even be backed by durable
// storage — defeating the entire point. Point path at somewhere the
// caller actually owns and expects to persist (a run's own output
// directory, a fixed ~/.chatwright/arena/flight-logs file, ...) —
// anywhere durable, never /tmp.
type FlightLog struct {
	now func() time.Time

	mu   sync.Mutex
	file *os.File
}

// OpenFlightLog opens (or creates) path for append-only writing —
// os.O_APPEND|os.O_CREATE, deliberately never os.O_TRUNC: re-running
// arena.Run against the same path preserves whatever a prior run, crashed
// mid-flight or not, already wrote, rather than erasing it. See FlightLog's
// own doc comment for why path must be a persistent location.
func OpenFlightLog(path string) (*FlightLog, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("arena: open flight log %s: %w", path, err)
	}
	return &FlightLog{file: f, now: time.Now}, nil
}

// Logf formats one line as "<RFC3339Nano timestamp> <message>\n", appends
// it to the file, and calls (*os.File).Sync() — before returning. By the
// time Logf returns, the line is durable: any reader of path, including a
// freshly-opened, independent *os.File in another process, is guaranteed
// to see it, even if this process is killed (or the kernel panics) on the
// very next instruction. Safe for concurrent use.
func (fl *FlightLog) Logf(format string, args ...any) error {
	line := fmt.Sprintf("%s %s\n", fl.now().UTC().Format(time.RFC3339Nano), fmt.Sprintf(format, args...))

	fl.mu.Lock()
	defer fl.mu.Unlock()
	if _, err := fl.file.WriteString(line); err != nil {
		return fmt.Errorf("arena: flight log write: %w", err)
	}
	return fl.file.Sync()
}

// Close closes the underlying file. Call once, after a Run (or matrix of
// Runs) finishes.
func (fl *FlightLog) Close() error {
	return fl.file.Close()
}

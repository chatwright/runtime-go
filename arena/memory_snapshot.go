package arena

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// memorySnapshotTimeout bounds each best-effort host-memory probe command
// (sysctl/memory_pressure below) — a hung probe must never hang an arena
// Run.
const memorySnapshotTimeout = 5 * time.Second

// memorySnapshotLookPath resolves a probe command's path — exec.LookPath by
// default, overridden in tests so this package's unit tests never depend on
// darwin-only tooling actually being on PATH.
var memorySnapshotLookPath = exec.LookPath

// memorySnapshotExec runs a probe command, returning its raw output —
// exec.CommandContext(...).Output() by default, overridden in tests
// alongside memorySnapshotLookPath.
var memorySnapshotExec = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// memorySnapshot returns a lightweight, one-line host-memory snapshot for
// the flight log's per-block-boundary line (runtime-go#8: "periodic memory
// snapshots"), or "" when no snapshot could be taken — this never returns
// an error and never blocks arena.Run: a missing or failing probe command
// degrades to omitting the line entirely, exactly like Loader's own
// degrade-never-fail contract.
//
// runtime.MemStats only covers this Go process's own heap, not host
// memory, and pulling in a new dependency is off the table (metered-tether
// constraint: stdlib only). So on darwin this shells out to two
// stdlib-reachable system tools already on every Mac — `sysctl -n
// vm.swapusage` (swap pressure) and `memory_pressure -Q` (a one-line free/
// pressure summary) — each guarded by exec.LookPath first. Every other
// GOOS has no equivalent stdlib-only probe available yet and this always
// returns "" there.
func memorySnapshot() string {
	if runtime.GOOS != "darwin" {
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), memorySnapshotTimeout)
	defer cancel()

	var parts []string
	if path, err := memorySnapshotLookPath("sysctl"); err == nil {
		if out, err := memorySnapshotExec(ctx, path, "-n", "vm.swapusage"); err == nil {
			if s := oneLine(out); s != "" {
				parts = append(parts, `swap="`+s+`"`)
			}
		}
	}
	if path, err := memorySnapshotLookPath("memory_pressure"); err == nil {
		if out, err := memorySnapshotExec(ctx, path, "-Q"); err == nil {
			if s := oneLine(out); s != "" {
				parts = append(parts, `pressure="`+s+`"`)
			}
		}
	}
	return strings.Join(parts, " ")
}

// oneLine collapses out's whitespace — including the embedded newlines
// `memory_pressure -Q` prints across multiple lines — into single spaces,
// so a probe's raw output can never split a flight-log entry across more
// than its own one physical line (FlightLog.Logf's whole append/fsync
// contract is per-line).
func oneLine(out []byte) string {
	return strings.Join(strings.Fields(string(out)), " ")
}

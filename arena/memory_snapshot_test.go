package arena

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// withMemorySnapshotSeams overrides memorySnapshotLookPath/memorySnapshotExec
// for the duration of the test, restoring the real exec-backed defaults
// afterwards — mirrors LMStudioLoader's own Exec/LookPath test seam so this
// package's unit tests never depend on darwin tooling actually existing on
// the machine running them.
func withMemorySnapshotSeams(t *testing.T, lookPath func(string) (string, error), run func(context.Context, string, ...string) ([]byte, error)) {
	t.Helper()
	origLookPath, origExec := memorySnapshotLookPath, memorySnapshotExec
	memorySnapshotLookPath, memorySnapshotExec = lookPath, run
	t.Cleanup(func() { memorySnapshotLookPath, memorySnapshotExec = origLookPath, origExec })
}

func TestMemorySnapshotDegradesWhenToolingAbsent(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("memorySnapshot only probes on darwin; always \"\" elsewhere")
	}
	withMemorySnapshotSeams(t,
		func(string) (string, error) { return "", exec.ErrNotFound },
		func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("memorySnapshotExec should not be called when LookPath fails")
			return nil, nil
		},
	)

	if got := memorySnapshot(); got != "" {
		t.Errorf("memorySnapshot() = %q, want \"\" when both probe tools are absent — never fail a run over a missing snapshot command", got)
	}
}

func TestMemorySnapshotDegradesWhenProbeCommandFails(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("memorySnapshot only probes on darwin; always \"\" elsewhere")
	}
	withMemorySnapshotSeams(t,
		func(name string) (string, error) { return "/usr/sbin/" + name, nil },
		func(context.Context, string, ...string) ([]byte, error) {
			return nil, exec.ErrNotFound
		},
	)

	if got := memorySnapshot(); got != "" {
		t.Errorf("memorySnapshot() = %q, want \"\" when the probe command itself errors", got)
	}
}

func TestMemorySnapshotIncludesProbedOutput(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("memorySnapshot only probes on darwin; always \"\" elsewhere")
	}
	withMemorySnapshotSeams(t,
		func(name string) (string, error) { return "/usr/sbin/" + name, nil },
		func(_ context.Context, name string, args ...string) ([]byte, error) {
			switch {
			case strings.HasSuffix(name, "sysctl"):
				return []byte("total = 1024.00M  used = 0.00M  free = 1024.00M  (encrypted)"), nil
			case strings.HasSuffix(name, "memory_pressure"):
				return []byte("System-wide memory free percentage: 62%"), nil
			}
			t.Fatalf("unexpected exec: %s %v", name, args)
			return nil, nil
		},
	)

	got := memorySnapshot()
	if !strings.Contains(got, "swap=") {
		t.Errorf("memorySnapshot() = %q, want a swap= field", got)
	}
	if !strings.Contains(got, "pressure=") {
		t.Errorf("memorySnapshot() = %q, want a pressure= field", got)
	}
}

// TestMemorySnapshotPartialWhenOneProbeMissing proves one probe's absence
// doesn't suppress the other — best-effort per probe, not all-or-nothing.
func TestMemorySnapshotPartialWhenOneProbeMissing(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("memorySnapshot only probes on darwin; always \"\" elsewhere")
	}
	withMemorySnapshotSeams(t,
		func(name string) (string, error) {
			if name == "memory_pressure" {
				return "", exec.ErrNotFound
			}
			return "/usr/sbin/" + name, nil
		},
		func(_ context.Context, name string, args ...string) ([]byte, error) {
			return []byte("total = 0.00M used = 0.00M free = 0.00M"), nil
		},
	)

	got := memorySnapshot()
	if !strings.Contains(got, "swap=") {
		t.Errorf("memorySnapshot() = %q, want the still-available swap= field", got)
	}
	if strings.Contains(got, "pressure=") {
		t.Errorf("memorySnapshot() = %q, want no pressure= field when that probe is absent", got)
	}
}

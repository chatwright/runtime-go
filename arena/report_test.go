package arena

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// fixtureModels builds a small, hand-built []ModelResult exercising every
// row shape WriteReport must handle: a clean model with a Verify-backed
// completion, a model whose one cell errored, and a model whose provider
// could not even be constructed — so metrics computation and report
// rendering are tested from fixture data, never a live network/CLI call.
func fixtureModels() []ModelResult {
	clean := ModelResult{
		Spec: ProviderSpec{Kind: KindOllama, Model: "qwen3.6:latest", BaseURL: "http://localhost:11434/v1"},
		Warmup: &WarmupResult{
			ColdStart: 9800 * time.Millisecond,
			Call:      CallRecord{Mode: "json_schema"},
		},
		Cells: []CellResult{
			{
				Repeat: 1, BundleName: "ollama-qwen3-6-latest-r1.chatwright.json",
				Wall: 10 * time.Second, StopReason: "goal-complete", PartStatus: "completed", TaskStatus: "completed",
				Steps: 4, InputTokens: 1000, OutputTokens: 100,
				Latencies:    []time.Duration{500 * time.Millisecond, 700 * time.Millisecond, 900 * time.Millisecond, 1200 * time.Millisecond},
				ActionCounts: map[string]int{"executed": 3, "task-completed": 1},
				ModeCounts:   map[string]int{"json_schema": 4},
				Calls:        []CallRecord{{Mode: "json_schema"}, {Mode: "json_schema"}, {Mode: "json_schema"}, {Mode: "json_schema"}},
				Verified:     true, VerifyDetail: "started, clicked English, acknowledged — all journal-verified",
			},
			{
				Repeat: 2, BundleName: "ollama-qwen3-6-latest-r2.chatwright.json",
				Wall: 12 * time.Second, StopReason: "budget-steps", PartStatus: "completed", TaskStatus: "active",
				Steps: 12, InputTokens: 15000, OutputTokens: 800,
				Latencies:    []time.Duration{600 * time.Millisecond, 5 * time.Second},
				ActionCounts: map[string]int{"executed": 11, "skipped-invalid": 1},
				ModeCounts:   map[string]int{"json_schema": 12},
				Calls:        oneErrorCall(12, "json_schema"),
				Verified:     false, VerifyDetail: "journal evidence incomplete: never sent an acknowledgement after the greeting changed",
			},
		},
	}

	withCellError := ModelResult{
		Spec:   ProviderSpec{Kind: KindLMStudio, Model: "google/gemma-4-e4b"},
		Warmup: &WarmupResult{ColdStart: 9500 * time.Millisecond, Call: CallRecord{Mode: "json_schema"}},
		Cells: []CellResult{
			{Repeat: 1, Err: errors.New("arena: scenario setup: boom")},
		},
	}

	providerFailed := ModelResult{
		Spec:        ProviderSpec{Kind: KindLMStudio, Model: "qwen/qwen3.6-27b"},
		ProviderErr: errors.New("actor/openai: no model: set Config.Model"),
	}

	return []ModelResult{clean, withCellError, providerFailed}
}

// oneErrorCall returns n CallRecords of mode, the last one carrying an
// error — the retry breakdown's transport-error source.
func oneErrorCall(n int, mode string) []CallRecord {
	calls := make([]CallRecord, n)
	for i := range calls {
		calls[i] = CallRecord{Mode: mode}
	}
	calls[n-1].Error = "context deadline exceeded"
	return calls
}

func TestPercentile(t *testing.T) {
	durs := []time.Duration{
		100 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond,
		400 * time.Millisecond, 500 * time.Millisecond,
	}
	if got := percentile(durs, 50); got != 300*time.Millisecond {
		t.Errorf("percentile(50) = %v, want 300ms", got)
	}
	if got := percentile(durs, 95); got != 500*time.Millisecond {
		t.Errorf("percentile(95) = %v, want 500ms", got)
	}
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("percentile(nil, 50) = %v, want 0", got)
	}
}

func TestRetryBreakdown(t *testing.T) {
	models := fixtureModels()
	skipped, noEffect, resFailed, transportErr := retryBreakdown(models[0].Cells)
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
	if noEffect != 0 {
		t.Errorf("noEffect = %d, want 0", noEffect)
	}
	if resFailed != 0 {
		t.Errorf("resFailed = %d, want 0", resFailed)
	}
	if transportErr != 1 {
		t.Errorf("transportErr = %d, want 1 (the deadline-exceeded call in repeat 2)", transportErr)
	}
}

func TestModeShares(t *testing.T) {
	models := fixtureModels()
	schemaN, fallbackN, otherN, total := modeShares(models[0].Cells)
	if total != 16 {
		t.Fatalf("total = %d, want 16 (4 + 12)", total)
	}
	if schemaN != 16 {
		t.Errorf("schemaN = %d, want 16", schemaN)
	}
	if fallbackN != 0 || otherN != 0 {
		t.Errorf("fallbackN=%d otherN=%d, want both 0", fallbackN, otherN)
	}
}

func TestWriteReportRendersEveryModelRowNeverSilently(t *testing.T) {
	results := Results{
		Scenario: ScenarioInfo{ID: "greetbot-language-onboarding", Version: "v1", Title: "Complete language onboarding and acknowledge the greeting"},
		Environment: Environment{
			OS: "darwin", Arch: "arm64", GoVersion: "go1.26.4", Date: time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC),
			Providers: []ProviderEnvironment{
				{Spec: ProviderSpec{Kind: KindOllama, Model: "qwen3.6:latest", ContextLength: 4096}, LoadResult: LoadResult{Performed: true, Note: "pre-loaded qwen3.6:latest, num_ctx=4096"}},
			},
		},
		Models: fixtureModels(),
	}

	var b strings.Builder
	if err := WriteReport(&b, results); err != nil {
		t.Fatalf("WriteReport() error = %v", err)
	}
	got := b.String()

	// The spec's required metric surface must all appear.
	for _, want := range []string{
		"## Environment",
		"## Headline comparison",
		"## Retry breakdown",
		"## Structured-output mode",
		"## Per-cell detail",
		"## Per-model narrative",
		"Cold-start",
		"context=4096 tokens",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q:\n%s", want, got)
		}
	}

	// Every declared model gets a row/section, even the two broken ones —
	// the exclusion policy's "never silently".
	for _, want := range []string{"ollama/qwen3.6:latest", "lmstudio/google/gemma-4-e4b", "lmstudio/qwen/qwen3.6-27b"} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not mention model %q — a model must never be silently dropped:\n%s", want, got)
		}
	}

	// The cell-level error and the provider-level error both surface as
	// explicit text, not an empty/omitted row.
	if !strings.Contains(got, "arena: scenario setup: boom") {
		t.Error("report does not surface the per-cell error")
	}
	if !strings.Contains(got, "no model: set Config.Model") {
		t.Error("report does not surface the provider-construction error")
	}

	// Bundle names are referenced per cell.
	if !strings.Contains(got, "ollama-qwen3-6-latest-r1.chatwright.json") {
		t.Error("report does not name the cell's bundle")
	}

	// The clean model's mismatched repeat 2 (self-declared active,
	// journal-verified false) is called out narratively.
	if !strings.Contains(got, "never sent an acknowledgement after the greeting changed") {
		t.Error("report narrative does not surface the VerifyDetail mismatch")
	}
}

func TestWriteReportVerifiedColumnIsNAWithoutScenarioVerify(t *testing.T) {
	results := Results{
		Scenario: ScenarioInfo{ID: "s", Version: "v1"},
		Environment: Environment{
			Date: time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC),
		},
		Models: []ModelResult{
			{
				Spec: ProviderSpec{Kind: KindOpenAICompat, Model: "gpt-nope"},
				Cells: []CellResult{
					{Repeat: 1, BundleName: "b1.chatwright.json", TaskStatus: "completed", PartStatus: "completed", StopReason: "goal-complete"},
				},
			},
		},
	}

	var b strings.Builder
	if err := WriteReport(&b, results); err != nil {
		t.Fatalf("WriteReport() error = %v", err)
	}
	got := b.String()
	if !strings.Contains(got, "| n/a |") {
		t.Errorf("report should show n/a for the Verified column when the scenario declares no Verify step:\n%s", got)
	}
}

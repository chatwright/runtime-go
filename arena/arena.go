// Package arena runs Chatwright's actor-model comparison matrix: the same
// Scenario (a goal plus a platform environment), the same budgets, across a
// declared set of provider/model configurations, N repeats each — see
// spec/ideas/actor-model-arena.md in the chatwright/chatwright standard
// repository for the arena this package implements (mandatory warm-up with
// cold-start as its own metric, right-sized context windows, a full retry
// breakdown, evidence over claims).
//
// Ported from the scratchpad harness that produced the first actor-model
// arena report (chatwright/backstage research/model-arena-2026-07-23): the
// same warm-up/right-sizing/retry-breakdown mechanics, restructured as a
// reusable public API rather than a pair of throwaway cmd/ binaries driven
// by shell scripting. The harness wrote a run bundle plus a JSON "sidecar"
// per cell (call-level detail a run bundle has no field for — see
// CallRecord) and read them back from disk to build a report; this package
// never writes anything to disk itself. Run returns everything — including
// every cell's full sdk.Bundle — as in-memory Results; a caller (the
// chatwright arena CLI subcommand) persists whatever it needs (bundles,
// the report, a machine-readable Results dump) into its own chosen
// directory layout. See Run and WriteReport.
//
// # Host stability on Apple Silicon
//
// Rapid large-model Metal load/unload cycling has triggered macOS GPU
// kernel panics during arena reruns on this hardware (chatwright/backstage
// research/model-arena-2026-07-23/crash-analysis.md): two panics, both an
// identical IOGPU/Metal driver signature ("Memory object unexpectedly not
// found in fPendingMemorySet" @IOGPUGroupMemory.cpp:219), observed 2/2 on
// macOS 26.5.2, Apple M5 Max 36GB unified memory, both during large-model
// (26B/27B) blocks. A third panic then occurred at idle, forty minutes
// after model activity had already finished cleanly — so model size alone
// is not a reliable predictor, and the machine is not safe to treat as
// "done" the moment inference stops. Until this is root-caused as a driver
// bug rather than a workload trigger: run any large-model matrix on Apple
// Silicon only attended, with RunOptions.FlightLog set to a persistent
// path (see FlightLog) — a kernel panic erases RAM, including whatever the
// model server itself was logging, so the flight log's fsync'd last line
// is what tells you which phase, and which model, was running when the
// machine went down.
package arena

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/goal"
	"chatwright.dev/runtime/observe"
	"chatwright.dev/runtime/run"
	"chatwright.dev/sdk"
)

// Matrix declares one arena run: the Scenario every cell attempts
// identically, the provider/model columns to compare, how many timed
// repeats each gets, and the Budgets every cell runs under.
type Matrix struct {
	// Scenario is the goal/environment every provider/repeat runs
	// identically — see Scenario, GreetbotScenario.
	Scenario Scenario
	// Providers is this matrix's declared column set, run in this order —
	// see ProviderSpec.
	Providers []ProviderSpec
	// Repeats is how many timed campaigns each provider runs, on top of
	// its own mandatory untimed warm-up. Must be >= 1.
	Repeats int
	// Budgets overrides every cell's goal.Budgets when non-zero (compared
	// against the zero value); the zero value leaves Scenario.Goal.Budgets
	// as declared. Applies uniformly across the whole matrix — the spec's
	// "identical budgets" requirement.
	Budgets goal.Budgets
}

// RunOptions configures Run beyond what Matrix declares: clock injection,
// timeouts, and the seams tests use to substitute a fake Provider/Loader
// for the real network/CLI ones.
type RunOptions struct {
	// Now supplies Run's notion of the current time, for every clock this
	// package's own logic needs: cold-start/wall timing, each cell's
	// run.Environment clock, and Results.Environment.Date. Nil uses
	// time.Now.
	Now func() time.Time

	// HTTPTimeout bounds one timed-repeat Propose call. <= 0 defaults to
	// 150s.
	HTTPTimeout time.Duration
	// WarmupTimeout bounds the mandatory untimed warm-up call — longer
	// than HTTPTimeout by default because a cold/JIT load can run long
	// (spec/ideas/actor-model-arena.md: "LM Studio loading a 27B exceeded
	// a 60s call timeout"). <= 0 defaults to 300s.
	WarmupTimeout time.Duration

	// Loaders maps a ProviderKind to the Loader Run uses for that kind's
	// per-model-block evict-others-then-right-size step — see Loader. Nil
	// uses DefaultLoaders(); a kind missing from the map gets NoopLoader.
	Loaders map[ProviderKind]Loader

	// ProviderFactory builds the actor.Provider Run drives for one
	// ProviderSpec — called exactly once per matrix column, and the
	// result reused for that column's mandatory warm-up and every one of
	// its timed repeats (a real actor/openai.Provider is a thin, stateless
	// HTTP wrapper — safe to reuse across calls; see
	// actor/openai.Provider.LastResponseFormatMode's own concurrency-safe
	// doc comment). Nil uses a default that builds an actor/openai.Provider
	// from the spec's BaseURL/Model/APIKey/MaxTokens. Tests override this
	// to substitute actor.NewScriptedProvider — see the package's e2e
	// test — so a whole matrix runs at zero cost and zero tokens, with no
	// real network or CLI dependency.
	ProviderFactory func(spec ProviderSpec) (actor.Provider, error)

	// Hardware is a free-text label for the report's environment block
	// (spec/ideas/actor-model-arena.md: entries "carry a declared
	// hardware/environment block") — e.g. "Apple M5 Max, 36GB unified
	// memory". Never auto-detected: left blank unless the caller supplies
	// it.
	Hardware string

	// ProgressWriter, when non-nil, receives one formatted stage line
	// (FormatProgressLine) per run.ProgressSnapshot emitted while running
	// each cell — spec/ideas/campaign-progress-reporting.md's "the arena
	// harness (per-cell progress and matrix position, e.g. 'model 2/4 ·
	// repeat 1/3 · task 1/2 · steps 5/12')". Nil (the zero value) means no
	// progress output — Run behaves exactly as before this field existed.
	ProgressWriter io.Writer

	// FlightLog, when non-nil, makes Run write one fsync'd line to it
	// before/around every consequential phase boundary — matrix start,
	// each model block's loader invocation and its actual outcome, warm-up
	// start/end (with cold-start seconds), each cell's start/end (repeat
	// index, outcome summary), block end (with a best-effort host-memory
	// snapshot), and matrix end. Nil (the default) disables flight logging
	// entirely — zero behavioural change, and Run never allocates or opens
	// anything on its account. See FlightLog and, in particular, its own
	// doc comment on why the path backing it must be persistent, never
	// /tmp: this exists to survive a kernel panic (chatwright/runtime-go#8).
	FlightLog *FlightLog
}

// logf writes one flight-log line when o.FlightLog is set, and does
// nothing otherwise — every Run call site uses this instead of calling
// o.FlightLog.Logf directly, so a nil FlightLog stays a true no-op with no
// scattered nil checks at each call site. A write/fsync failure is
// swallowed (best-effort diagnostics, matching Loader's own degrade-never-
// fail contract): a flight-log problem must never abort a Run that could
// otherwise keep producing real evidence.
func (o RunOptions) logf(format string, args ...any) {
	if o.FlightLog == nil {
		return
	}
	_ = o.FlightLog.Logf(format, args...)
}

func (o RunOptions) clock() func() time.Time {
	if o.Now != nil {
		return o.Now
	}
	return time.Now
}

func (o RunOptions) httpTimeout() time.Duration {
	if o.HTTPTimeout > 0 {
		return o.HTTPTimeout
	}
	return 150 * time.Second
}

func (o RunOptions) warmupTimeout() time.Duration {
	if o.WarmupTimeout > 0 {
		return o.WarmupTimeout
	}
	return 300 * time.Second
}

func (o RunOptions) loaderFor(kind ProviderKind) Loader {
	loaders := o.Loaders
	if loaders == nil {
		loaders = DefaultLoaders()
	}
	if l, ok := loaders[kind]; ok {
		return l
	}
	return NoopLoader{}
}

func (o RunOptions) providerFactory() func(ProviderSpec) (actor.Provider, error) {
	if o.ProviderFactory != nil {
		return o.ProviderFactory
	}
	timeout := o.httpTimeout()
	now := o.clock()
	return func(spec ProviderSpec) (actor.Provider, error) {
		return defaultProviderFactory(spec, timeout, now)
	}
}

// ScenarioInfo records which scenario (and version) a Results was run
// against — spec/ideas/actor-model-arena.md's groundwork for a canonical-
// scenario registry: two Results sharing the same ID+Version are
// apples-to-apples comparable; anything else is not.
type ScenarioInfo struct {
	ID, Version, Title string
}

// ProviderEnvironment records one matrix column's declared configuration
// plus whatever its per-block Loader actually managed to do.
type ProviderEnvironment struct {
	Spec       ProviderSpec
	LoadResult LoadResult
}

// Environment is Results' declared hardware/software/scenario context — the
// spec's "entries carry a declared hardware/environment block".
type Environment struct {
	Hardware  string
	OS, Arch  string
	GoVersion string
	Date      time.Time
	Providers []ProviderEnvironment
}

// WarmupResult is one provider's mandatory untimed warm-up call — the
// spec's own required metric: "the measured cold-start/load time is
// reported as its own metric, never mixed into proposal latency".
type WarmupResult struct {
	ColdStart time.Duration
	Call      CallRecord
	// Err is set when the warm-up call itself errored (e.g. an
	// empty-reply model) — still recorded, never silently dropped:
	// cold-start is measured either way, up to the point of the error.
	Err error
}

// CellResult is one timed repeat's complete outcome.
type CellResult struct {
	Repeat int

	// BundleName is a suggested, collision-free filename for Bundle — the
	// spec's "per-cell bundle names" report requirement. A caller
	// persisting bundles should use it verbatim, e.g.
	// filepath.Join(outDir, "bundles", cell.BundleName).
	BundleName string
	// Bundle is this cell's complete sdk.Bundle. Run never writes it to
	// disk — see the package doc comment.
	Bundle sdk.Bundle
	Wall   time.Duration

	StopReason   string
	PartStatus   string
	TaskStatus   string
	Steps        int
	InputTokens  int
	OutputTokens int

	// Calls is every Propose call this cell made, successful or not — the
	// retry breakdown's transport-error source (see CallRecord).
	Calls []CallRecord
	// ActionCounts tallies actor.ActionOutcomeKind values (as their JSON
	// string form, e.g. "skipped-invalid") across this cell's LoopEvents.
	ActionCounts map[string]int
	// ModeCounts tallies each successful Propose call's response_format
	// mode (see CallRecord.Mode); a call with no reported mode is not
	// counted.
	ModeCounts map[string]int
	// Latencies is each LoopEvent's Usage.Latency, in the loop's own
	// order — the p50/p95 proposal-latency metric's raw material.
	Latencies []time.Duration

	// Verified/VerifyDetail carry Scenario.Verify's verdict, when the
	// Scenario declares one (see Scenario.Verify) — both zero value
	// (false, "") when it does not.
	Verified     bool
	VerifyDetail string

	// Err is set when this cell's own setup or run.Run.Execute call
	// returned a hard, Run-level configuration error (see
	// run.Run.Execute's own doc comment on what that error is reserved
	// for) — a harness/arena bug, never a model's own data point. Every
	// other field is zero when Err is set: this cell contributed no
	// evidence.
	Err error
}

// ModelResult is one matrix column's complete outcome: its declared spec,
// its mandatory warm-up, and every timed repeat it ran.
type ModelResult struct {
	Spec   ProviderSpec
	Warmup *WarmupResult
	Cells  []CellResult
	// ProviderErr is set when Run could not even construct this
	// provider's actor.Provider (a ProviderFactory failure) — recorded as
	// a data point rather than aborting the whole matrix, per the spec's
	// exclusion policy: a model leaves the report only with a recorded,
	// evidence-linked reason, "never silently".
	ProviderErr error
}

// Results is everything one arena Run produced.
type Results struct {
	Scenario    ScenarioInfo
	Environment Environment
	// Models is one ModelResult per Matrix.Providers entry, in that same
	// declared order — every declared provider always gets an entry here,
	// however its block went (see ModelResult.ProviderErr,
	// CellResult.Err).
	Models []ModelResult
}

// Run executes matrix's cells sequentially: one per-model block per
// Matrix.Providers entry, in declared order — a Loader evict/right-size
// hook, then one untimed mandatory warm-up, then Matrix.Repeats timed
// campaigns. Sequential by design (spec/ideas/actor-model-arena.md, "Not
// Doing": "Parallel arena execution — sequential first; timing fidelity
// beats speed").
//
// A per-provider failure (ProviderFactory) is recorded on that
// ModelResult and Run moves on to the next provider; a per-cell setup or
// run.Run.Execute configuration error is recorded on that CellResult the
// same way. Run's own returned error is reserved for a Matrix that cannot
// be executed at all (no Scenario.Setup, no Providers, Repeats < 1) or a
// Loader's own hard error — never for a model's own data point: every
// declared provider that Run does start always gets a ModelResult,
// however it went, per the spec's exclusion policy ("never silently").
func Run(ctx context.Context, matrix Matrix, opts RunOptions) (Results, error) {
	if matrix.Scenario.Setup == nil {
		return Results{}, fmt.Errorf("arena: Matrix.Scenario.Setup is nil")
	}
	if len(matrix.Providers) == 0 {
		return Results{}, fmt.Errorf("arena: Matrix.Providers is empty")
	}
	if matrix.Repeats < 1 {
		return Results{}, fmt.Errorf("arena: Matrix.Repeats must be >= 1, got %d", matrix.Repeats)
	}

	now := opts.clock()
	newProvider := opts.providerFactory()
	goalDef := effectiveGoal(matrix.Scenario, matrix.Budgets)

	results := Results{
		Scenario: ScenarioInfo{ID: matrix.Scenario.ID, Version: matrix.Scenario.Version, Title: matrix.Scenario.Title},
		Environment: Environment{
			Hardware: opts.Hardware, OS: runtime.GOOS, Arch: runtime.GOARCH,
			GoVersion: runtime.Version(), Date: now(),
		},
	}

	opts.logf("arena: phase=matrix-start scenario=%s@%s providers=%s", matrix.Scenario.ID, matrix.Scenario.Version, providerLabels(matrix.Providers))

	for modelIndex, spec := range matrix.Providers {
		opts.logf("arena: phase=loader-invoke model=%s kind=%s", spec.label(), string(spec.Kind))
		loadResult, err := opts.loaderFor(spec.Kind).Load(ctx, spec)
		if err != nil {
			opts.logf("arena: phase=load model=%s ctx=%d err=%q", spec.label(), spec.ContextLength, err.Error())
			return results, fmt.Errorf("arena: load %s: %w", spec.label(), err)
		}
		opts.logf("arena: phase=load model=%s ctx=%d performed=%t note=%q", spec.label(), spec.ContextLength, loadResult.Performed, loadResult.Note)
		results.Environment.Providers = append(results.Environment.Providers, ProviderEnvironment{Spec: spec, LoadResult: loadResult})

		model := runProviderModelBlock(ctx, matrix, spec, opts, goalDef, now, newProvider, matrixPosition{
			modelIndex: modelIndex + 1, modelCount: len(matrix.Providers), repeatCount: matrix.Repeats,
		})
		results.Models = append(results.Models, model)

		// Block end is its own flight-log phase before the next model's
		// loader-invoke: the founder's own third panic hit forty minutes
		// after a block finished cleanly, at idle — so the memory snapshot
		// belongs right here, at the boundary, not only during activity.
		opts.logf("arena: phase=block-end model=%s", spec.label())
		if snap := memorySnapshot(); snap != "" {
			opts.logf("arena: phase=memory %s", snap)
		}
	}

	opts.logf("arena: phase=matrix-end models=%d", len(results.Models))

	return results, nil
}

// runProviderModelBlock runs one matrix column's mandatory warm-up plus its
// Matrix.Repeats timed cells, flight-logging each phase boundary as it
// goes. Split out of Run so the surrounding loop's own load/block-end/
// memory-snapshot flight-log lines bracket it uniformly, whether this block
// completes normally or returns early on a ProviderFactory failure.
func runProviderModelBlock(ctx context.Context, matrix Matrix, spec ProviderSpec, opts RunOptions, goalDef goal.Goal, now func() time.Time, newProvider func(ProviderSpec) (actor.Provider, error), pos matrixPosition) ModelResult {
	provider, err := newProvider(spec)
	if err != nil {
		return ModelResult{Spec: spec, ProviderErr: err}
	}

	model := ModelResult{Spec: spec}

	opts.logf("arena: phase=warmup-start model=%s", spec.label())
	model.Warmup = runWarmup(ctx, provider, now, opts.warmupTimeout(), goalDef)
	opts.logf("arena: phase=warmup-end model=%s cold_start_s=%.2f err=%q", spec.label(), model.Warmup.ColdStart.Seconds(), errText(model.Warmup.Err))

	for repeat := 1; repeat <= matrix.Repeats; repeat++ {
		pos.repeat = repeat
		opts.logf("arena: phase=cell-start model=%s repeat=%d", spec.label(), repeat)
		cell := runCell(ctx, matrix.Scenario, provider, spec, goalDef, repeat, now, opts.ProgressWriter, pos)
		opts.logf("arena: phase=cell-end model=%s repeat=%d outcome=%q", spec.label(), repeat, cellOutcome(cell))
		model.Cells = append(model.Cells, cell)
	}

	return model
}

// providerLabels renders specs' labels for the matrix-start flight-log
// line, e.g. "[lmstudio/gemma-4-26b, ollama/qwen3.6]".
func providerLabels(specs []ProviderSpec) string {
	labels := make([]string, len(specs))
	for i, s := range specs {
		labels[i] = s.label()
	}
	return "[" + strings.Join(labels, ", ") + "]"
}

// errText returns err.Error(), or "" when err is nil — so a flight-log
// line can always carry an err= field unconditionally rather than omitting
// it depending on success/failure.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// cellOutcome summarises one CellResult for its flight-log cell-end line —
// enough to tell, from the flight log alone, roughly how a cell went
// without needing the full bundle.
func cellOutcome(c CellResult) string {
	if c.Err != nil {
		return "error: " + firstLine(c.Err.Error())
	}
	return fmt.Sprintf("task=%s stop=%s verified=%t", c.TaskStatus, c.StopReason, c.Verified)
}

// effectiveGoal returns scenario.Goal with its Budgets replaced by override
// when override is non-zero (see Matrix.Budgets) — a value copy, so no two
// cells ever share mutable state a caller could observe cross-cell
// mutation through.
func effectiveGoal(scenario Scenario, override goal.Budgets) goal.Goal {
	g := scenario.Goal
	if override != (goal.Budgets{}) {
		g.Budgets = override
	}
	return g
}

// runWarmup issues exactly one Propose call — the spec's mandatory warm-up
// rule — shaped like the real campaign's first-ever prompt (an empty
// observation, the scenario's own goal/task context), so its latency is a
// faithful cold-start/load measurement: reported as its own metric, never
// mixed into a timed cell's proposal latencies. Ported from the scratchpad
// harness's cmd/run-cell/main.go runWarmup.
func runWarmup(ctx context.Context, provider actor.Provider, now func() time.Time, timeout time.Duration, g goal.Goal) *WarmupResult {
	rec := newRecordingProvider(provider, now)

	prompt := actor.Prompt{GoalID: g.ID, GoalTitle: g.Title, Observation: observe.Observation{Sequence: 1}}
	if len(g.Tasks) > 0 {
		task := g.Tasks[0]
		prompt.TaskID, prompt.TaskTitle, prompt.TaskSuccessCriteria = task.ID, task.Title, task.SuccessCriteria
	}

	callCtx, cancel := context.WithTimeout(ctx, timeout+20*time.Second)
	defer cancel()

	start := now()
	_, _, err := rec.Propose(callCtx, prompt)
	elapsed := now().Sub(start)

	wr := &WarmupResult{ColdStart: elapsed, Err: err}
	if records := rec.Records(); len(records) > 0 {
		wr.Call = records[0]
	}
	return wr
}

// matrixPosition names one cell's coordinates within Run's matrix — the
// arena-level context FormatProgressLine prepends to a run.ProgressSnapshot
// (spec/ideas/campaign-progress-reporting.md's "model 2/4 · repeat 1/3 ·
// task 1/2 · steps 5/12").
type matrixPosition struct {
	modelIndex, modelCount int
	repeat, repeatCount    int
}

// runCell runs one timed campaign — one Matrix repeat — against a fresh
// Scenario.Setup session, assembling a full sdk.Bundle exactly the way the
// scratchpad harness's cmd/run-cell/main.go runCell did (the same
// run.NewAIGoalPart / run.Run / run.AssembleBundleRun path every other
// chatwright runtime bundle writer uses), except it never writes the
// bundle to disk — see the package doc comment. When progressWriter is
// non-nil, one formatted stage line (FormatProgressLine) is written per
// run.ProgressSnapshot this cell's run.Run emits.
func runCell(ctx context.Context, scenario Scenario, provider actor.Provider, spec ProviderSpec, g goal.Goal, repeat int, now func() time.Time, progressWriter io.Writer, pos matrixPosition) CellResult {
	session, err := scenario.Setup()
	if err != nil {
		return CellResult{Repeat: repeat, Err: fmt.Errorf("arena: scenario setup: %w", err)}
	}
	defer session.Close()

	rec := newRecordingProvider(provider, now)

	part := run.NewAIGoalPart(scenario.ID, scenario.Title, "", run.AIGoalPartInput{
		ActorID: "ai-agent", Goal: g, Provider: rec,
		Config: actor.Config{ChatID: session.ChatID, User: session.User},
	})

	runID := fmt.Sprintf("%s-r%d", cellSlug(spec), repeat)
	r := run.Run{
		ID:          runID,
		Environment: run.Environment{Emulator: session.Emulator, ChatIDs: []int64{session.ChatID}, Now: now},
		Parts:       []run.Part{part},
	}
	if progressWriter != nil {
		r.OnProgress = func(snap run.ProgressSnapshot) {
			_, _ = io.WriteString(progressWriter, formatProgressLine(spec, pos, snap)+"\n")
		}
	}

	start := now()
	result, err := r.Execute(ctx)
	wall := now().Sub(start)
	if err != nil {
		return CellResult{Repeat: repeat, Wall: wall, Calls: rec.Records(), Err: fmt.Errorf("arena: run.Execute: %w", err)}
	}

	entries, err := session.Emulator.Journal(session.ChatID)
	if err != nil {
		return CellResult{Repeat: repeat, Wall: wall, Calls: rec.Records(), Err: fmt.Errorf("arena: read journal: %w", err)}
	}

	actors := []sdk.Actor{
		{
			ID: "ai-agent", Type: sdk.ActorAIAgent, Name: spec.Model,
			PlatformIdentities: map[string]sdk.PlatformIdentity{
				"telegram": {UserID: session.User.ID, FirstName: session.User.FirstName},
			},
			Provider: &sdk.ActorProvider{Name: "openai-compat", ModelIDs: []string{spec.Model}},
		},
		session.BotActor,
	}
	chats := []sdk.ChatJournal{run.WireJournal(session.ChatID, entries)}

	bundleRun := run.AssembleBundleRun(run.AssembleBundleRunInput{
		RunID: runID, Platform: "telegram", EndpointProfile: sdk.EndpointProfilePlatformEmulated,
		Actors: actors, Chats: chats, Result: result,
	})

	b := sdk.Bundle{
		Format: sdk.FormatV1,
		Metadata: sdk.Metadata{
			CreatedAt:         now().UTC(),
			ChatwrightVersion: sdk.ModuleVersion(),
			Author:            &sdk.Author{Name: "chatwright-arena"},
		},
		Runs: []sdk.Run{bundleRun},
	}

	cell := CellResult{
		Repeat: repeat, BundleName: bundleFileName(spec, repeat), Bundle: b, Wall: wall,
		Calls: rec.Records(), ActionCounts: map[string]int{}, ModeCounts: map[string]int{},
	}
	for _, call := range cell.Calls {
		if call.Mode != "" {
			cell.ModeCounts[call.Mode]++
		}
	}

	if len(result.Parts) > 0 {
		p := result.Parts[0]
		cell.PartStatus = string(p.Status)
		if p.AIGoal != nil {
			cell.StopReason = p.AIGoal.Report.StopReason
			cell.Steps = p.AIGoal.Report.Steps
			cell.InputTokens = p.AIGoal.Report.Usage.InputTokens
			cell.OutputTokens = p.AIGoal.Report.Usage.OutputTokens
			if len(p.AIGoal.Report.Tasks) > 0 {
				cell.TaskStatus = p.AIGoal.Report.Tasks[0].Status
			}
			for _, ev := range p.AIGoal.Events {
				cell.Latencies = append(cell.Latencies, ev.Usage.Latency)
				cell.ActionCounts[string(ev.Action.Kind)]++
			}
		}
	}

	if scenario.Verify != nil {
		vr := scenario.Verify(entries)
		cell.Verified, cell.VerifyDetail = vr.Verified, vr.Detail
	}

	return cell
}

func cellSlug(spec ProviderSpec) string {
	return slugify(string(spec.Kind) + "-" + spec.Model)
}

func bundleFileName(spec ProviderSpec, repeat int) string {
	return fmt.Sprintf("%s-r%d.chatwright.json", cellSlug(spec), repeat)
}

func slugify(s string) string {
	r := strings.NewReplacer("/", "-", ":", "-", ".", "-", " ", "-")
	return r.Replace(s)
}

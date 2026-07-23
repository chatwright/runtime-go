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
package arena

import (
	"context"
	"fmt"
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

	for _, spec := range matrix.Providers {
		loadResult, err := opts.loaderFor(spec.Kind).Load(ctx, spec)
		if err != nil {
			return results, fmt.Errorf("arena: load %s: %w", spec.label(), err)
		}
		results.Environment.Providers = append(results.Environment.Providers, ProviderEnvironment{Spec: spec, LoadResult: loadResult})

		provider, err := newProvider(spec)
		if err != nil {
			results.Models = append(results.Models, ModelResult{Spec: spec, ProviderErr: err})
			continue
		}

		model := ModelResult{Spec: spec}
		model.Warmup = runWarmup(ctx, provider, now, opts.warmupTimeout(), goalDef)

		for repeat := 1; repeat <= matrix.Repeats; repeat++ {
			model.Cells = append(model.Cells, runCell(ctx, matrix.Scenario, provider, spec, goalDef, repeat, now))
		}
		results.Models = append(results.Models, model)
	}

	return results, nil
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

// runCell runs one timed campaign — one Matrix repeat — against a fresh
// Scenario.Setup session, assembling a full sdk.Bundle exactly the way the
// scratchpad harness's cmd/run-cell/main.go runCell did (the same
// run.NewAIGoalPart / run.Run / run.AssembleBundleRun path every other
// chatwright runtime bundle writer uses), except it never writes the
// bundle to disk — see the package doc comment.
func runCell(ctx context.Context, scenario Scenario, provider actor.Provider, spec ProviderSpec, g goal.Goal, repeat int, now func() time.Time) CellResult {
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

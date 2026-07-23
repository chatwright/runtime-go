// Package run is Chatwright's part-composition runtime: it executes an
// ordered sequence of Parts — deterministic scenario fragments and ai-goal
// actor-loop passages — over one shared Environment (one platform.Emulator,
// one cast, one continuous journal), exactly the shape
// spec/ideas/hybrid-runs.md describes: "a run is an ordered sequence of
// parts ... over one environment, one cast, one continuous journal."
//
// A Run never duplicates journal content or re-runs mechanics that already
// exist elsewhere in this module: a deterministic Part executes an existing
// cw.Fragment via cw.InvokeFragment (provenance retained —
// see DeterministicOutcome); an ai-goal Part drives goal.CampaignState and
// actor.Loop exactly as the frozen campaign/bundle end-to-end tests do,
// scoped to that Part's own goal.Goal and goal.Budgets. Run.Execute captures
// each Part's journal boundary (per chat: first entry index + entry count)
// by snapshotting platform.Emulator.Journal before and after the Part runs
// — the "platform Journal seam" hybrid-runs.md calls for — and
// AssembleBundleRun (see bundle.go) turns the result into a sdk.Run,
// extending SingleAIGoalRun's single-part mapping to however many Parts
// actually ran, converting this runtime's internal types to the sdk's wire
// shapes along the way (see wire.go — the sdk owns every wire shape). A
// plain campaign is exactly a Run with one ai-goal Part, so the campaign
// execution path is fully expressible through this same layer, per
// hybrid-runs.md's MVP scope.
//
// Every timestamp this package's own runtime state needs — the run
// ceiling's elapsed-duration check, and each ai-goal Part's
// goal.CampaignState/actor.Loop clock — comes from Environment.Now, never
// time.Now, mirroring the rest of this module's injected-clock convention
// (goal.NewCampaignState, actor.Config.Now, sdk.Metadata.CreatedAt).
package run

import (
	"context"
	"errors"
	"fmt"
	"time"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/campaign"
	"chatwright.dev/runtime/cw"
	"chatwright.dev/runtime/goal"
	"chatwright.dev/runtime/observe"
	"chatwright.dev/runtime/platform"
	"chatwright.dev/sdk"
)

// ErrNilEmulator means Environment.Emulator was nil.
var ErrNilEmulator = errors.New("run: Environment.Emulator is nil")

// ErrNilClock means Environment.Now was nil.
var ErrNilClock = errors.New("run: Environment.Now is nil")

// ErrNoChatIDs means Environment.ChatIDs was empty. A Run cannot compute any
// Part's journal boundary without knowing which chats its journal spans —
// see Environment's own doc comment.
var ErrNoChatIDs = errors.New("run: Environment.ChatIDs is empty")

// Environment is the one platform.Emulator — plus its declared chat IDs and
// its injected clock — every Part in a Run executes over: the "one
// environment, one cast, one continuous journal" spec/ideas/hybrid-runs.md
// requires. ChatIDs names every chat this run's journal spans; Run.Execute
// snapshots each of them (via Emulator.Journal) immediately before and after
// every Part to compute that Part's sdk.JournalBoundary (see
// diffBoundary), so a chat a given Part never touches at all contributes no
// boundary entry for that Part. A Run never infers ChatIDs on its own —
// deterministic Parts execute an opaque cw.Fragment closure this
// package cannot introspect, so there is no way to discover which chats it
// touched short of the caller declaring them up front.
type Environment struct {
	// Emulator is the shared platform.Emulator every Part acts against.
	Emulator platform.Emulator
	// ChatIDs is every chat this run's journal spans, in the order boundary
	// entries are reported. Must be non-empty — see ErrNoChatIDs.
	ChatIDs []int64
	// Now supplies this run's notion of the current time — see the package
	// doc comment for why this is never time.Now internally. Must not be
	// nil — see ErrNilClock.
	Now func() time.Time
}

// PartStatus is one declared Part's outcome once a Run has finished (or
// stopped short of reaching it). It is a string type, not an int enum, so
// it marshals to human-readable JSON if a caller chooses to persist a
// Result, matching AGENTS.md's JSON-artefact convention.
type PartStatus string

// Part statuses. See PartStatus.
const (
	// PartCompleted: the Part executed and produced no error of its own.
	// For an ai-goal Part this says nothing about whether every task
	// succeeded — read AIGoalSection.Report for that — only that the loop
	// ran to a stop without a hard runtime error and the run ceiling never
	// tripped inside it.
	PartCompleted PartStatus = "completed"
	// PartFailed: the Part executed but its own mechanism reported a
	// failure — a deterministic Part's cw.Fragment.Execute returned
	// a non-nil error, or an ai-goal Part's actor.Loop.RunTask returned an
	// unexpected error. See PartOutcome.Err.
	PartFailed PartStatus = "failed"
	// PartCeilingStopped: an ai-goal Part was still executing when the
	// Run's RunCeiling tripped; the Part's own goal.CampaignState never
	// itself stopped. See PartOutcome.CeilingTrip.
	PartCeilingStopped PartStatus = "ceiling-stopped"
	// PartAborted: the Part never executed at all — a prior Part's
	// FailurePolicyAbort, or a RunCeiling trip (which always aborts,
	// regardless of any Part's FailurePolicy — see RunCeiling), stopped the
	// Run before it was reached.
	PartAborted PartStatus = "aborted"
	// PartCoverageGap: the Part never executed at all — a prior
	// deterministic Part failed under FailurePolicyCoverageGap. Distinct
	// from PartAborted so a caller can tell "the run gave up entirely" from
	// "this specific passage is an acknowledged coverage gap, opted into by
	// the failed part's own FailurePolicy".
	PartCoverageGap PartStatus = "coverage-gap"
)

// FailurePolicy declares what a Run does when one Part's own execution
// fails (PartFailed — see PartStatus): abort the whole Run, or mark every
// subsequent Part a coverage gap (PartCoverageGap) without executing it.
// hybrid-runs.md frames this around a failed deterministic Part
// specifically ("a failed deterministic part may abort the run or mark
// subsequent dependent parts as coverage gaps"); this package applies the
// same mechanism uniformly to an ai-goal Part's own hard runtime error too,
// for consistency — a genuinely deliberate generalisation, not scope creep,
// since nothing in hybrid-runs.md restricts it to one Part kind.
//
// The zero value ("") behaves exactly like FailurePolicyAbort — see
// effective. This is a deliberate, documented choice, not an accidental
// default: silently downgrading a hard failure into "the rest of the run
// continues, and everything after is an acknowledged gap" must be opted
// into explicitly (FailurePolicyCoverageGap); an unset FailurePolicy never
// silently changes what a Run does.
type FailurePolicy string

// Failure policies. See FailurePolicy.
const (
	// FailurePolicyAbort stops the Run immediately: no subsequent Part
	// executes, and none is recorded as a coverage gap either — see
	// PartAborted.
	FailurePolicyAbort FailurePolicy = "abort"
	// FailurePolicyCoverageGap marks every subsequent Part PartCoverageGap
	// (never executed) instead of aborting outright.
	FailurePolicyCoverageGap FailurePolicy = "coverage-gap"
)

// effective returns p, or FailurePolicyAbort if p is the zero value — see
// FailurePolicy's own doc comment for why this is the documented default,
// not a silent one.
func (p FailurePolicy) effective() FailurePolicy {
	if p == "" {
		return FailurePolicyAbort
	}
	return p
}

// Part is one declared passage of a Run: an id, a title, a kind-scoped
// payload built by NewDeterministicPart or NewAIGoalPart, and a
// FailurePolicy. The zero Part (constructed by hand rather than through
// either builder) has neither payload set; Run.Execute reports a clear
// error for it rather than silently doing nothing.
type Part struct {
	// ID is caller-supplied and only needs to be unique within its Run —
	// mirrors sdk.Part.ID, since AssembleBundleRun copies it there
	// verbatim for every Part that actually executed.
	ID string
	// Title is an optional human-readable label — see sdk.Part.Title.
	Title string
	// Kind discriminates this Part's payload — sdk.PartKindDeterministic
	// or sdk.PartKindAIGoal, reusing the sdk's own wire vocabulary rather
	// than a parallel enum.
	Kind sdk.PartKind
	// FailurePolicy declares what happens to the rest of the Run if this
	// Part fails — see FailurePolicy.
	FailurePolicy FailurePolicy

	deterministic *deterministicSpec
	aiGoal        *aiGoalSpec
}

// Run is an ordered sequence of Parts executing over one Environment — the
// canonical "run" of docs/glossary.md. Execute runs every Part in order
// (see Execute) and returns a Result describing what happened to each one.
type Run struct {
	// ID identifies this Run — required (cw.NewExecutionContext,
	// which threads deterministic-Part provenance across the whole Run,
	// needs a non-empty root path/definition name) and, conventionally, the
	// same value later given to sdk.Run.ID when assembling evidence.
	ID string
	// Environment is the one platform.Emulator (plus its declared chat IDs
	// and clock) every Part executes over — see Environment.
	Environment Environment
	// Parts is this Run's ordered passages — see Part, NewDeterministicPart,
	// NewAIGoalPart.
	Parts []Part
	// Ceiling optionally aggregates ai-goal step/cost/duration usage across
	// every Part in the Run, on top of each ai-goal Part's own
	// goal.Budgets — see RunCeiling. The zero value means "no run-level
	// ceiling", mirroring goal.Budgets' own "zero means unlimited"
	// convention.
	Ceiling RunCeiling
}

// Result is everything Run.Execute produced: every Part that actually ran,
// in order, plus every Part the Run stopped short of reaching.
type Result struct {
	// RunID mirrors Run.ID.
	RunID string
	// Parts is every Part that actually executed (PartCompleted,
	// PartFailed or PartCeilingStopped — see PartStatus), in the order it
	// ran. AssembleBundleRun turns this directly into a sdk.Run's Parts.
	Parts []PartOutcome
	// Skipped is every Part the Run never reached (PartAborted or
	// PartCoverageGap), in declared order, once execution stopped short.
	Skipped []SkippedPart
	// CeilingTrip is set once Ceiling ever tripped during this Run, nil
	// otherwise — mirrors whichever PartOutcome.CeilingTrip actually
	// tripped it, kept at Result level too so a caller never has to scan
	// Parts to learn whether the run-level ceiling fired at all.
	CeilingTrip *CeilingTrip
}

// SkippedPart names one declared Part the Run never executed, and why.
type SkippedPart struct {
	PartID string
	Status PartStatus // PartAborted or PartCoverageGap.
}

// PartOutcome is one executed Part's complete result.
type PartOutcome struct {
	PartID string
	Title  string
	Kind   sdk.PartKind
	Status PartStatus
	// Err is set when Status is PartFailed: the deterministic Fragment's
	// own error, or the ai-goal Loop.RunTask's own error.
	Err error
	// Boundary is this Part's slice of the run-level journal, computed by
	// diffBoundary from Environment snapshots taken immediately before and
	// after the Part ran.
	Boundary sdk.JournalBoundary
	// Deterministic is set for a deterministic Part: the retained
	// cw.FragmentInvocation provenance (definition, steps,
	// checkpoints, branches, failures) — see DeterministicOutcome.
	Deterministic *DeterministicOutcome
	// AIGoal is set for an ai-goal Part that at least started running (any
	// status except one the Run never reached): the same shape
	// sdk.AIGoalSection carries, ready to attach to a sdk.Part
	// verbatim.
	AIGoal *sdk.AIGoalSection
	// CeilingTrip is set exactly on the one PartOutcome where Run.Ceiling
	// tripped (Status will be PartCeilingStopped), nil otherwise.
	CeilingTrip *CeilingTrip
}

// Execute runs every declared Part in order over r.Environment: deterministic
// Parts via cw.InvokeFragment, ai-goal Parts via goal.CampaignState
// and actor.Loop, in the same sequence and against the same shared journal —
// see the package doc comment. It stops the Run early (recording every
// remaining Part in Result.Skipped) when a Part fails and its FailurePolicy
// says to (FailurePolicyAbort or FailurePolicyCoverageGap — see Part.
// FailurePolicy), or when r.Ceiling trips (which always halts the Run,
// regardless of any Part's FailurePolicy).
//
// The returned error is reserved for a Run-level configuration problem (a
// nil Environment field, an empty/duplicate Part ID, an ai-goal Part
// referencing a chat Environment.ChatIDs never declared, or a constructor
// failure — goal.NewCampaignState/actor.NewLoop rejecting a malformed
// Goal/Config) — never for a Part's own execution failure, which is
// recorded as PartFailed in the returned Result instead. On such an error,
// the returned Result still carries every Part that completed before the
// problem was found.
func (r Run) Execute(ctx context.Context) (Result, error) {
	result := Result{RunID: r.ID}

	if err := r.validate(); err != nil {
		return result, err
	}

	root, err := cw.NewExecutionContext(cw.Definition{Name: r.ID}, r.ID)
	if err != nil {
		return result, fmt.Errorf("run: %w", err)
	}

	tracker := &ceilingTracker{ceiling: r.Ceiling, now: r.Environment.Now, runStart: r.Environment.Now()}

	halted := PartStatus("") // once non-empty (PartAborted or PartCoverageGap), every remaining Part is skipped with that status.
	for _, part := range r.Parts {
		if halted != "" {
			result.Skipped = append(result.Skipped, SkippedPart{PartID: part.ID, Status: halted})
			continue
		}

		before, err := r.snapshotCounts()
		if err != nil {
			return result, err
		}

		var outcome PartOutcome
		switch part.Kind {
		case sdk.PartKindDeterministic:
			outcome, err = r.runDeterministic(root, part)
		case sdk.PartKindAIGoal:
			outcome, err = r.runAIGoal(ctx, part, tracker)
		default:
			err = fmt.Errorf("run: part %q has unknown kind %q", part.ID, part.Kind)
		}
		if err != nil {
			return result, err
		}

		after, err := r.snapshotCounts()
		if err != nil {
			return result, err
		}
		outcome.Boundary = diffBoundary(r.Environment.ChatIDs, before, after)
		result.Parts = append(result.Parts, outcome)

		if outcome.CeilingTrip != nil {
			result.CeilingTrip = outcome.CeilingTrip
			halted = PartAborted // a ceiling trip is a hard stop; no FailurePolicy choice applies.
			continue
		}
		if outcome.Status == PartFailed {
			switch part.FailurePolicy.effective() {
			case FailurePolicyCoverageGap:
				halted = PartCoverageGap
			default: // FailurePolicyAbort
				halted = PartAborted
			}
		}
	}

	return result, nil
}

// validate checks r's own configuration — everything Execute needs to be
// non-nil/non-empty/consistent before it runs a single Part. It deliberately
// does not (and cannot) validate a deterministic Part's own Fragment: that
// closure is opaque until it runs.
func (r Run) validate() error {
	if r.ID == "" {
		return errors.New("run: Run.ID is empty")
	}
	if r.Environment.Emulator == nil {
		return ErrNilEmulator
	}
	if r.Environment.Now == nil {
		return ErrNilClock
	}
	if len(r.Environment.ChatIDs) == 0 {
		return ErrNoChatIDs
	}
	declared := make(map[int64]bool, len(r.Environment.ChatIDs))
	for _, id := range r.Environment.ChatIDs {
		declared[id] = true
	}

	seenIDs := make(map[string]bool, len(r.Parts))
	for _, part := range r.Parts {
		if part.ID == "" {
			return errors.New("run: a Part has an empty ID")
		}
		if seenIDs[part.ID] {
			return fmt.Errorf("run: duplicate part id %q", part.ID)
		}
		seenIDs[part.ID] = true

		if part.Kind == sdk.PartKindAIGoal && part.aiGoal != nil && !declared[part.aiGoal.cfg.ChatID] {
			return fmt.Errorf("run: part %q targets chat %d, which Environment.ChatIDs does not declare", part.ID, part.aiGoal.cfg.ChatID)
		}
	}
	return nil
}

// snapshotCounts reads every declared chat's current journal length from
// r.Environment.Emulator — the "before"/"after" snapshot diffBoundary turns
// into a Part's sdk.JournalBoundary.
func (r Run) snapshotCounts() (map[int64]int, error) {
	counts := make(map[int64]int, len(r.Environment.ChatIDs))
	for _, id := range r.Environment.ChatIDs {
		entries, err := r.Environment.Emulator.Journal(id)
		if err != nil {
			return nil, fmt.Errorf("run: journal chat %d: %w", id, err)
		}
		counts[id] = len(entries)
	}
	return counts, nil
}

// diffBoundary turns a before/after snapshot pair into the
// sdk.JournalBoundary the Part between them covers, in chatIDs' declared
// order. A chat with no new entries during the Part contributes no
// ChatBoundary at all — JournalBoundary.Chats only ever names chats the Part
// actually produced journal content in.
func diffBoundary(chatIDs []int64, before, after map[int64]int) sdk.JournalBoundary {
	boundary := sdk.JournalBoundary{}
	for _, id := range chatIDs {
		count := after[id] - before[id]
		if count <= 0 {
			continue
		}
		boundary.Chats = append(boundary.Chats, sdk.ChatBoundary{ChatID: id, FirstEntry: before[id], EntryCount: count})
	}
	return boundary
}

// runDeterministic executes part's cw.Fragment via
// cw.InvokeFragment against root, retaining its provenance in
// DeterministicOutcome. A Fragment error is reported as PartFailed, never as
// runDeterministic's own returned error — see Execute's doc comment on what
// that error is reserved for.
func (r Run) runDeterministic(root *cw.ExecutionContext, part Part) (PartOutcome, error) {
	outcome := PartOutcome{PartID: part.ID, Title: part.Title, Kind: part.Kind}
	if part.deterministic == nil {
		return outcome, fmt.Errorf("run: deterministic part %q declares no fragment (build it with NewDeterministicPart)", part.ID)
	}

	evidence, err := part.deterministic.invoke(root)
	outcome.Deterministic = &DeterministicOutcome{
		Definition:  evidence.definition,
		Steps:       evidence.steps,
		Checkpoints: evidence.checkpoints,
		Branches:    evidence.branches,
		Failures:    evidence.failures,
	}
	if err != nil {
		outcome.Status = PartFailed
		outcome.Err = err
		return outcome, nil
	}
	outcome.Status = PartCompleted
	return outcome, nil
}

// runAIGoal drives part's ai-goal payload through goal.CampaignState and
// actor.Loop, task by task (mirroring actor.Loop.RunCampaign's own
// eligible-task iteration — see nextEligibleTask), checking tracker after
// every task so a run-level ceiling can interrupt a multi-task Part between
// tasks. This is deliberately the finest granularity Loop's public API
// exposes an external interruption point at: RunTask always runs a task to
// its own conclusion (terminal status, or that task's own goal.Budgets
// stopping the whole per-part campaign) before returning, so "the ceiling
// trips mid-part" means "between two tasks of the same Part", not
// mid-task — interrupting inside a single task's own step loop would need a
// per-iteration hook actor.Loop does not expose, and adding one is out of
// this package's scope.
func (r Run) runAIGoal(ctx context.Context, part Part, tracker *ceilingTracker) (PartOutcome, error) {
	outcome := PartOutcome{PartID: part.ID, Title: part.Title, Kind: part.Kind}
	if part.aiGoal == nil {
		return outcome, fmt.Errorf("run: ai-goal part %q declares no goal/provider (build it with NewAIGoalPart)", part.ID)
	}
	spec := part.aiGoal

	cfg := spec.cfg
	cfg.Now = r.Environment.Now // the run's clock always wins — see AIGoalPartInput.Config's own doc comment.

	engine := observe.NewEngine(r.Environment.Emulator, observe.ChatRef{ChatID: cfg.ChatID})
	campaignState, err := goal.NewCampaignState(spec.goalDef, r.Environment.Now)
	if err != nil {
		return outcome, fmt.Errorf("run: part %q: %w", part.ID, err)
	}
	loop, err := actor.NewLoop(spec.provider, engine, r.Environment.Emulator, campaignState, spec.goalDef, cfg)
	if err != nil {
		return outcome, fmt.Errorf("run: part %q: %w", part.ID, err)
	}

	priorEvents := 0
	for !campaignState.Stopped() {
		taskID, ok := nextEligibleTask(spec.goalDef, campaignState)
		if !ok {
			break
		}
		if _, err := loop.RunTask(ctx, taskID); err != nil {
			outcome.Status = PartFailed
			outcome.Err = err
			outcome.AIGoal = buildAIGoalSection(spec, campaignState, loop)
			return outcome, nil
		}

		events := loop.Events()
		delta := events[priorEvents:]
		priorEvents = len(events)
		if trip := tracker.record(part.ID, len(delta), sumCost(delta)); trip != nil {
			outcome.Status = PartCeilingStopped
			outcome.CeilingTrip = trip
			outcome.AIGoal = buildAIGoalSection(spec, campaignState, loop)
			return outcome, nil
		}
	}

	outcome.Status = PartCompleted
	outcome.AIGoal = buildAIGoalSection(spec, campaignState, loop)
	return outcome, nil
}

// buildAIGoalSection assembles the sdk.AIGoalSection a Part's outcome
// carries — the same pieces (Goal, actor id, events, retained observations,
// campaign.Assemble's Report) the frozen bundle end-to-end test assembles by
// hand, gathered here once so runAIGoal never duplicates it across its three
// return points (clean completion, mid-task error, ceiling trip). The
// runtime values are converted to the sdk's wire shapes at exactly this
// packing point — see wire.go; the sdk owns every wire shape.
func buildAIGoalSection(spec *aiGoalSpec, campaignState *goal.CampaignState, loop *actor.Loop) *sdk.AIGoalSection {
	events := loop.Events()
	report := campaign.Assemble(campaign.AssembleInput{Goal: spec.goalDef, Campaign: campaignState.Snapshot(), Events: events})
	return &sdk.AIGoalSection{
		Goal:         wireGoal(spec.goalDef),
		ActorID:      spec.actorID,
		Events:       wireLoopEvents(events),
		Observations: wireRetainedObservations(loop.Observations()),
		Report:       wireReport(report),
	}
}

// nextEligibleTask returns the first task (in g.Tasks order) currently
// eligible to be activated — mirrors actor.Loop's own unexported
// nextEligibleTask, reimplemented here because runAIGoal needs to interleave
// a ceiling check between tasks, which Loop.RunCampaign's all-in-one loop
// does not expose a seam for.
func nextEligibleTask(g goal.Goal, campaignState *goal.CampaignState) (string, bool) {
	for _, t := range g.Tasks {
		if eligible, err := campaignState.Eligible(t.ID); err == nil && eligible {
			return t.ID, true
		}
	}
	return "", false
}

// sumCost totals the Usage.Cost of every event, treating a nil Cost as 0 —
// mirrors goal.CampaignState.RecordCost's own per-call accrual, aggregated
// here across a batch of events at once.
func sumCost(events []actor.LoopEvent) float64 {
	var total float64
	for _, e := range events {
		if e.Usage.Cost != nil {
			total += *e.Usage.Cost
		}
	}
	return total
}

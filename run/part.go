package run

import (
	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/cw"
	"chatwright.dev/runtime/goal"
	"chatwright.dev/sdk"
)

// deterministicSpec is a declared deterministic Part's kind-scoped payload:
// a closure, built by NewDeterministicPart, that invokes a specific
// cw.Fragment[T] (with T resolved at declaration time) against
// whatever *cw.ExecutionContext Run.Execute supplies later. Part
// itself must stay a plain, non-generic struct so a Run can hold Parts built
// from different Fragment input types in one []Part — this closure is where
// that type is erased, the same way http.HandlerFunc erases a concrete
// handler signature.
type deterministicSpec struct {
	invoke func(parent *cw.ExecutionContext) (fragmentEvidence, error)
}

// fragmentEvidence is the subset of a cw.FragmentInvocation[T]
// runDeterministic retains once T has been erased — see DeterministicOutcome,
// which this is copied into verbatim.
type fragmentEvidence struct {
	definition  cw.Definition
	steps       []cw.StepEvidence
	checkpoints []cw.CheckpointEvidence
	branches    []cw.BranchEvidence
	failures    []cw.FailureEvidence
}

// DeterministicOutcome is a deterministic Part's retained provenance: the
// cw.FragmentInvocation evidence cw.InvokeFragment produced,
// carried through unconverted so nothing this package's own execution
// invented can be mistaken for evidence the fragment itself recorded.
type DeterministicOutcome struct {
	Definition  cw.Definition
	Steps       []cw.StepEvidence
	Checkpoints []cw.CheckpointEvidence
	Branches    []cw.BranchEvidence
	Failures    []cw.FailureEvidence
}

// NewDeterministicPart declares a Part whose deterministic passage executes
// fragment (with the given effective inputs) via cw.InvokeFragment
// against the Run's own root *cw.ExecutionContext when Run.Execute
// reaches it — the existing scenario-composition contract, provenance
// retained (see DeterministicOutcome), not a parallel mechanism.
//
// fragment.Execute drives the Run's shared Environment however the fragment
// needs to — typically the raw platform.Emulator primitives (SubmitText,
// SubmitClick, WaitForMessage, WaitForEdit — the same seam actor.Loop's own
// Actuator uses) rather than a testing.TB-bound cw.Chat, since a
// composed Run has no *testing.T of its own to hand a Chat and must report
// failure by returning an error, not by calling t.Fatalf. Fragment[T]'s
// signature (func(*ExecutionContext, T) error) carries no environment
// reference of its own, so the emulator/chat identity fragment.Execute acts
// against must be closed over from the call site, exactly like
// example_test.go's greetScenario closes over a *cw.Chatwright.
//
// policy governs what happens to the rest of the Run if this Part's
// fragment returns an error — see FailurePolicy.
func NewDeterministicPart[T any](id, title string, policy FailurePolicy, fragment cw.Fragment[T], inputs cw.EffectiveInputs[T]) Part {
	return Part{
		ID: id, Title: title, Kind: sdk.PartKindDeterministic, FailurePolicy: policy,
		deterministic: &deterministicSpec{
			invoke: func(parent *cw.ExecutionContext) (fragmentEvidence, error) {
				invocation, err := cw.InvokeFragment(parent, id, fragment, inputs)
				return fragmentEvidence{
					definition:  invocation.Definition,
					steps:       invocation.Steps,
					checkpoints: invocation.Checkpoints,
					branches:    invocation.Branches,
					failures:    invocation.Failures,
				}, err
			},
		},
	}
}

// aiGoalSpec is a declared ai-goal Part's kind-scoped payload.
type aiGoalSpec struct {
	actorID  string
	goalDef  goal.Goal
	provider actor.Provider
	cfg      actor.Config
}

// AIGoalPartInput is everything NewAIGoalPart needs to declare an ai-goal
// Part.
type AIGoalPartInput struct {
	// ActorID references the roster sdk.Actor that runs this Part's
	// loop — becomes sdk.AIGoalSection.ActorID verbatim.
	ActorID string
	// Goal is this Part's own goal.Goal, including its own goal.Budgets —
	// each ai-goal Part carries its own budgets, independent of any
	// Run.Ceiling (see RunCeiling).
	Goal goal.Goal
	// Provider proposes this Part's actions — any actor.Provider, including
	// actor.NewScriptedProvider for a zero-token, fully deterministic Part.
	Provider actor.Provider
	// Config configures the actor.Loop this Part runs. ChatID and User are
	// required (also used to construct this Part's own observe.Engine).
	// Config.Now is always overwritten with the owning Run's
	// Environment.Now before the loop starts — a caller-set value here is
	// never consulted, so it may be left nil. Config.OnProgress is
	// likewise overwritten (to a closure forwarding into the owning Run's
	// own OnProgress, with this Part's position added) whenever the Run
	// declares one — see Run.OnProgress; a caller-set value here is only
	// consulted when the Run itself declares no OnProgress.
	Config actor.Config
}

// NewAIGoalPart declares a Part whose ai-goal passage drives in.Provider
// through a fresh goal.CampaignState/actor.Loop pair for in.Goal — the same
// mechanism the frozen campaign/bundle end-to-end tests drive directly,
// scoped to one Part of a larger Run. policy governs what happens to the
// rest of the Run if this Part's loop returns an unexpected error (not a
// budget stop or a task simply failing/blocking — those are ordinary,
// evidenced outcomes visible in the Part's assembled Report, never
// PartFailed) — see FailurePolicy.
func NewAIGoalPart(id, title string, policy FailurePolicy, in AIGoalPartInput) Part {
	return Part{
		ID: id, Title: title, Kind: sdk.PartKindAIGoal, FailurePolicy: policy,
		aiGoal: &aiGoalSpec{actorID: in.ActorID, goalDef: in.Goal, provider: in.Provider, cfg: in.Config},
	}
}

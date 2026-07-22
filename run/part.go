package run

import (
	"github.com/chatwright/chatwright"
	"github.com/chatwright/chatwright/actor"
	"github.com/chatwright/chatwright/bundle"
	"github.com/chatwright/chatwright/goal"
)

// deterministicSpec is a declared deterministic Part's kind-scoped payload:
// a closure, built by NewDeterministicPart, that invokes a specific
// chatwright.Fragment[T] (with T resolved at declaration time) against
// whatever *chatwright.ExecutionContext Run.Execute supplies later. Part
// itself must stay a plain, non-generic struct so a Run can hold Parts built
// from different Fragment input types in one []Part — this closure is where
// that type is erased, the same way http.HandlerFunc erases a concrete
// handler signature.
type deterministicSpec struct {
	invoke func(parent *chatwright.ExecutionContext) (fragmentEvidence, error)
}

// fragmentEvidence is the subset of a chatwright.FragmentInvocation[T]
// runDeterministic retains once T has been erased — see DeterministicOutcome,
// which this is copied into verbatim.
type fragmentEvidence struct {
	definition  chatwright.Definition
	steps       []chatwright.StepEvidence
	checkpoints []chatwright.CheckpointEvidence
	branches    []chatwright.BranchEvidence
	failures    []chatwright.FailureEvidence
}

// DeterministicOutcome is a deterministic Part's retained provenance: the
// chatwright.FragmentInvocation evidence chatwright.InvokeFragment produced,
// carried through unconverted so nothing this package's own execution
// invented can be mistaken for evidence the fragment itself recorded.
type DeterministicOutcome struct {
	Definition  chatwright.Definition
	Steps       []chatwright.StepEvidence
	Checkpoints []chatwright.CheckpointEvidence
	Branches    []chatwright.BranchEvidence
	Failures    []chatwright.FailureEvidence
}

// NewDeterministicPart declares a Part whose deterministic passage executes
// fragment (with the given effective inputs) via chatwright.InvokeFragment
// against the Run's own root *chatwright.ExecutionContext when Run.Execute
// reaches it — the existing scenario-composition contract, provenance
// retained (see DeterministicOutcome), not a parallel mechanism.
//
// fragment.Execute drives the Run's shared Environment however the fragment
// needs to — typically the raw platform.Emulator primitives (SubmitText,
// SubmitClick, WaitForMessage, WaitForEdit — the same seam actor.Loop's own
// Actuator uses) rather than a testing.TB-bound chatwright.Chat, since a
// composed Run has no *testing.T of its own to hand a Chat and must report
// failure by returning an error, not by calling t.Fatalf. Fragment[T]'s
// signature (func(*ExecutionContext, T) error) carries no environment
// reference of its own, so the emulator/chat identity fragment.Execute acts
// against must be closed over from the call site, exactly like
// example_test.go's greetScenario closes over a *chatwright.Chatwright.
//
// policy governs what happens to the rest of the Run if this Part's
// fragment returns an error — see FailurePolicy.
func NewDeterministicPart[T any](id, title string, policy FailurePolicy, fragment chatwright.Fragment[T], inputs chatwright.EffectiveInputs[T]) Part {
	return Part{
		ID: id, Title: title, Kind: bundle.PartKindDeterministic, FailurePolicy: policy,
		deterministic: &deterministicSpec{
			invoke: func(parent *chatwright.ExecutionContext) (fragmentEvidence, error) {
				invocation, err := chatwright.InvokeFragment(parent, id, fragment, inputs)
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
	// ActorID references the roster bundle.Actor that runs this Part's
	// loop — becomes bundle.AIGoalSection.ActorID verbatim.
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
	// never consulted, so it may be left nil.
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
		ID: id, Title: title, Kind: bundle.PartKindAIGoal, FailurePolicy: policy,
		aiGoal: &aiGoalSpec{actorID: in.ActorID, goalDef: in.Goal, provider: in.Provider, cfg: in.Config},
	}
}

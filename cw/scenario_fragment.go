package chatwright

import (
	"errors"
	"strings"
)

// Fragment is a reusable typed scenario definition. CloneInputs must return a
// detached copy: Chatwright calls it separately for execution and evidence so a
// fragment cannot mutate its caller's inputs or its recorded effective inputs.
type Fragment[T any] struct {
	Definition  Definition
	CloneInputs func(T) T
	Execute     func(*ExecutionContext, T) error
}

// FragmentInvocation is the source-linked evidence for one fragment call. Its
// locally produced records do not include evidence from nested fragments.
type FragmentInvocation[T any] struct {
	Path        InvocationPath
	ParentPath  InvocationPath
	Definition  Definition
	Inputs      EffectiveInputs[T]
	Steps       []StepEvidence
	Checkpoints []CheckpointEvidence
	Branches    []BranchEvidence
	Failures    []FailureEvidence
}

// InvokeFragment executes a reusable fragment beneath parent. invocationName
// is part of the machine path, allowing the same definition and checkpoint
// labels to be used more than once without identity collisions.
func InvokeFragment[T any](
	parent *ExecutionContext,
	invocationName string,
	fragment Fragment[T],
	inputs EffectiveInputs[T],
) (FragmentInvocation[T], error) {
	if parent == nil {
		return FragmentInvocation[T]{}, errors.New("chatwright: fragment parent context is nil")
	}
	if strings.TrimSpace(fragment.Definition.Name) == "" {
		return FragmentInvocation[T]{}, errors.New("chatwright: fragment definition name is empty")
	}
	if fragment.CloneInputs == nil {
		return FragmentInvocation[T]{}, errors.New("chatwright: fragment CloneInputs is nil")
	}
	if fragment.Execute == nil {
		return FragmentInvocation[T]{}, errors.New("chatwright: fragment Execute is nil")
	}

	path, err := parent.path.child(invocationName)
	if err != nil {
		return FragmentInvocation[T]{}, err
	}
	evidenceInputs := fragment.CloneInputs(inputs.Value)
	executionInputs := fragment.CloneInputs(inputs.Value)
	inputSources := cloneInputSources(inputs.Sources)

	context := &ExecutionContext{
		path:       path,
		definition: fragment.Definition,
		current:    parent.inheritedCheckpoint(),
	}
	err = fragment.Execute(context, executionInputs)
	if err != nil {
		context.RecordFailure(err, fragment.Definition.Source)
	}

	evidence := context.Evidence()
	invocation := FragmentInvocation[T]{
		Path:       evidence.Path,
		ParentPath: parent.Path(),
		Definition: evidence.Definition,
		Inputs: EffectiveInputs[T]{
			Value:   evidenceInputs,
			Sources: inputSources,
		},
		Steps:       evidence.Steps,
		Checkpoints: evidence.Checkpoints,
		Branches:    evidence.Branches,
		Failures:    evidence.Failures,
	}
	if err == nil {
		parent.adoptCheckpoint(context.inheritedCheckpoint())
	}
	return invocation, err
}

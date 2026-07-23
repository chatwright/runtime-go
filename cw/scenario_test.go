package cw

import (
	"errors"
	"reflect"
	"testing"
)

type groceriesInput struct {
	Titles []string
}

func cloneGroceriesInput(input groceriesInput) groceriesInput {
	return groceriesInput{Titles: append([]string(nil), input.Titles...)}
}

func mustExecutionContext(t *testing.T, definition Definition, path ...string) *ExecutionContext {
	t.Helper()
	context, err := NewExecutionContext(definition, path...)
	if err != nil {
		t.Fatalf("NewExecutionContext() error = %v", err)
	}
	return context
}

func TestFragmentInvocationRecordsParentPathSourceAndInputs(t *testing.T) {
	parentSource := SourceReference{
		URI:      "https://github.com/sneat-co/sneat-bots/blob/parent123/scenarios/new_user.go#L20",
		Revision: "parent123",
	}
	fragmentSource := SourceReference{
		URI:      "https://github.com/sneat-co/sneat-bots/blob/fragment456/scenarios/list_items.go#L30",
		Revision: "fragment456",
	}
	inputSource := InputSource{
		Kind:      "fixture",
		Reference: "four-groceries",
		Source: SourceReference{
			URI:      "https://github.com/sneat-co/sneat-bots/blob/fixture789/scenarios/fixtures.go#L12",
			Revision: "fixture789",
		},
	}
	parent := mustExecutionContext(t, Definition{Name: "new-user", Source: parentSource}, "listus", "new-user")
	stepSource := SourceReference{URI: fragmentSource.URI + "-step", Revision: fragmentSource.Revision}
	checkpointSource := SourceReference{URI: fragmentSource.URI + "-checkpoint", Revision: fragmentSource.Revision}
	branchSource := SourceReference{URI: fragmentSource.URI + "-branch", Revision: fragmentSource.Revision}
	wantErr := errors.New("semantic item assertion failed")
	fragment := Fragment[groceriesInput]{
		Definition:  Definition{Name: "list-items-modification", Source: fragmentSource},
		CloneInputs: cloneGroceriesInput,
		Execute: func(context *ExecutionContext, _ groceriesInput) error {
			context.RecordStep("add baseline groceries", stepSource)
			if _, err := context.Checkpoint("few-items-added", checkpointSource); err != nil {
				return err
			}
			context.RecordBranch("add-new-and-existing", branchSource)
			return wantErr
		},
	}

	invocation, err := InvokeFragment(parent, "list-items", fragment, EffectiveInputs[groceriesInput]{
		Value:   groceriesInput{Titles: []string{"milk", "bread", "eggs", "apples"}},
		Sources: map[string]InputSource{"titles": inputSource},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("InvokeFragment() error = %v, want %v", err, wantErr)
	}
	if got, want := invocation.ParentPath.String(), "listus/new-user"; got != want {
		t.Errorf("ParentPath = %q, want %q", got, want)
	}
	if got, want := invocation.Path.String(), "listus/new-user/list-items"; got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
	if invocation.Definition != fragment.Definition {
		t.Errorf("Definition = %#v, want %#v", invocation.Definition, fragment.Definition)
	}
	if !reflect.DeepEqual(invocation.Inputs.Value.Titles, []string{"milk", "bread", "eggs", "apples"}) {
		t.Errorf("Inputs.Value.Titles = %v", invocation.Inputs.Value.Titles)
	}
	if got := invocation.Inputs.Sources["titles"]; got != inputSource {
		t.Errorf("Inputs.Sources[titles] = %#v, want %#v", got, inputSource)
	}
	if len(invocation.Steps) != 1 || invocation.Steps[0].Source != stepSource {
		t.Errorf("Steps = %#v", invocation.Steps)
	}
	if len(invocation.Checkpoints) != 1 || invocation.Checkpoints[0].Source != checkpointSource {
		t.Errorf("Checkpoints = %#v", invocation.Checkpoints)
	}
	if len(invocation.Branches) != 1 || invocation.Branches[0].Source != branchSource {
		t.Errorf("Branches = %#v", invocation.Branches)
	}
	if len(invocation.Failures) != 1 || invocation.Failures[0].Message != wantErr.Error() || invocation.Failures[0].Source != fragmentSource {
		t.Errorf("Failures = %#v", invocation.Failures)
	}
}

func TestFragmentInputsAreIsolated(t *testing.T) {
	definition := Definition{
		Name: "list-items-modification",
		Source: SourceReference{
			URI:      "https://github.com/sneat-co/sneat-bots/blob/abc123/scenarios/list_items.go",
			Revision: "abc123",
		},
	}
	fragment := Fragment[groceriesInput]{
		Definition:  definition,
		CloneInputs: cloneGroceriesInput,
		Execute: func(_ *ExecutionContext, input groceriesInput) error {
			input.Titles[0] = "changed inside fragment"
			input.Titles = append(input.Titles, "bananas")
			return nil
		},
	}
	shared := groceriesInput{Titles: []string{"milk", "bread"}}
	sharedSources := map[string]InputSource{
		"titles": {Kind: "fixture", Reference: "baseline-groceries"},
	}
	firstParent := mustExecutionContext(t, Definition{Name: "new-user"}, "listus", "new-user")
	secondParent := mustExecutionContext(t, Definition{Name: "existing-user"}, "listus", "existing-user")

	first, err := InvokeFragment(firstParent, "list-items", fragment, EffectiveInputs[groceriesInput]{
		Value:   shared,
		Sources: sharedSources,
	})
	if err != nil {
		t.Fatalf("first InvokeFragment() error = %v", err)
	}
	second, err := InvokeFragment(secondParent, "list-items", fragment, EffectiveInputs[groceriesInput]{
		Value:   shared,
		Sources: sharedSources,
	})
	if err != nil {
		t.Fatalf("second InvokeFragment() error = %v", err)
	}

	want := []string{"milk", "bread"}
	if !reflect.DeepEqual(shared.Titles, want) {
		t.Errorf("caller inputs = %v, want %v", shared.Titles, want)
	}
	if !reflect.DeepEqual(first.Inputs.Value.Titles, want) {
		t.Errorf("first effective inputs = %v, want %v", first.Inputs.Value.Titles, want)
	}
	if !reflect.DeepEqual(second.Inputs.Value.Titles, want) {
		t.Errorf("second effective inputs = %v, want %v", second.Inputs.Value.Titles, want)
	}
	if fragment.Definition != definition {
		t.Errorf("fragment definition mutated: %#v", fragment.Definition)
	}
	first.Inputs.Value.Titles[0] = "caller-mutated evidence"
	if second.Inputs.Value.Titles[0] != "milk" {
		t.Errorf("mutating first evidence changed second evidence: %v", second.Inputs.Value.Titles)
	}
	first.Inputs.Sources["titles"] = InputSource{Kind: "override", Reference: "first-only"}
	if got := second.Inputs.Sources["titles"]; got != sharedSources["titles"] {
		t.Errorf("mutating first input sources changed second: %#v", got)
	}
	if got := sharedSources["titles"]; got.Kind != "fixture" {
		t.Errorf("invocation mutated caller input sources: %#v", got)
	}
}

func TestCheckpointIdentityIsQualifiedByInvocationPath(t *testing.T) {
	source := SourceReference{URI: "https://example.test/list-items.go", Revision: "abc123"}
	fragment := Fragment[struct{}]{
		Definition:  Definition{Name: "list-items-modification", Source: source},
		CloneInputs: func(input struct{}) struct{} { return input },
		Execute: func(context *ExecutionContext, _ struct{}) error {
			_, err := context.Checkpoint("few-items-added", source)
			return err
		},
	}
	newUser := mustExecutionContext(t, Definition{Name: "new-user"}, "listus", "new-user")
	existingUser := mustExecutionContext(t, Definition{Name: "existing-user"}, "listus", "existing-user")

	newInvocation, err := InvokeFragment(newUser, "list-items", fragment, EffectiveInputs[struct{}]{})
	if err != nil {
		t.Fatalf("new-user InvokeFragment() error = %v", err)
	}
	existingInvocation, err := InvokeFragment(existingUser, "list-items", fragment, EffectiveInputs[struct{}]{})
	if err != nil {
		t.Fatalf("existing-user InvokeFragment() error = %v", err)
	}

	newCheckpoint := newInvocation.Checkpoints[0]
	existingCheckpoint := existingInvocation.Checkpoints[0]
	if newCheckpoint.Name != existingCheckpoint.Name {
		t.Fatalf("display names differ: %q != %q", newCheckpoint.Name, existingCheckpoint.Name)
	}
	if newCheckpoint.ID == existingCheckpoint.ID {
		t.Fatalf("qualified checkpoint IDs collided: %q", newCheckpoint.ID)
	}
	if got, want := newCheckpoint.ID, CheckpointID("listus/new-user/list-items/checkpoints/few-items-added"); got != want {
		t.Errorf("new-user checkpoint ID = %q, want %q", got, want)
	}
	if got, want := existingCheckpoint.ID, CheckpointID("listus/existing-user/list-items/checkpoints/few-items-added"); got != want {
		t.Errorf("existing-user checkpoint ID = %q, want %q", got, want)
	}
}

func TestCheckpointLineageCrossesParentAndFragment(t *testing.T) {
	parentSource := SourceReference{URI: "https://example.test/new-user.go", Revision: "parent123"}
	fragmentSource := SourceReference{URI: "https://example.test/list-items.go", Revision: "fragment456"}
	parent := mustExecutionContext(t, Definition{Name: "new-user", Source: parentSource}, "listus", "new-user")
	onboarding, err := parent.Checkpoint("onboarding-complete", parentSource)
	if err != nil {
		t.Fatalf("parent.Checkpoint() error = %v", err)
	}
	fragment := Fragment[struct{}]{
		Definition:  Definition{Name: "list-items-modification", Source: fragmentSource},
		CloneInputs: func(input struct{}) struct{} { return input },
		Execute: func(context *ExecutionContext, _ struct{}) error {
			_, err := context.Checkpoint("few-items-added", fragmentSource)
			return err
		},
	}

	invocation, err := InvokeFragment(parent, "list-items", fragment, EffectiveInputs[struct{}]{})
	if err != nil {
		t.Fatalf("InvokeFragment() error = %v", err)
	}
	fewItems := invocation.Checkpoints[0]
	if got, want := fewItems.ParentID, onboarding.ID; got != want {
		t.Errorf("few-items parent = %q, want %q", got, want)
	}
	if got, want := fewItems.Lineage, []CheckpointID{onboarding.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("few-items lineage = %v, want %v", got, want)
	}
	if onboarding.InvocationPath != parent.Path() {
		t.Errorf("onboarding invocation path = %q, want %q", onboarding.InvocationPath, parent.Path())
	}
	if fewItems.InvocationPath != invocation.Path {
		t.Errorf("few-items invocation path = %q, want %q", fewItems.InvocationPath, invocation.Path)
	}

	afterFragment, err := parent.Checkpoint("after-fragment", parentSource)
	if err != nil {
		t.Fatalf("after-fragment Checkpoint() error = %v", err)
	}
	if got, want := afterFragment.Lineage, []CheckpointID{onboarding.ID, fewItems.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("after-fragment lineage = %v, want %v", got, want)
	}
}

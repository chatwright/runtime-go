package branching

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type fakeHandle struct{ generation, branch string }

type fakeHolder struct {
	name       string
	log        *[]string
	captureErr error
	releaseErr error
	branchErr  error
	finishErr  error
	captures   int
	releases   int
	branches   int
	finishes   int
	source     *fakeHandle
}

func (h *fakeHolder) Capability() Capability {
	return Capability{Provider: "fake-" + h.name, Version: "v1"}
}

func (h *fakeHolder) Capture(context.Context) (HolderCheckpoint, error) {
	h.captures++
	if h.log != nil {
		*h.log = append(*h.log, "capture:"+h.name)
	}
	cp := &fakeCheckpoint{holder: h, generation: h.name + "-g"}
	if h.captureErr != nil {
		return cp, h.captureErr
	}
	return cp, nil
}

type fakeCheckpoint struct {
	holder     *fakeHolder
	generation string
}

func (c *fakeCheckpoint) Generation() string { return c.generation }
func (c *fakeCheckpoint) Release(context.Context) error {
	c.holder.releases++
	if c.holder.log != nil {
		*c.holder.log = append(*c.holder.log, "release:"+c.holder.name)
	}
	return c.holder.releaseErr
}
func (c *fakeCheckpoint) Branch(context.Context) (HolderBranch, error) {
	c.holder.branches++
	if c.holder.log != nil {
		*c.holder.log = append(*c.holder.log, "branch:"+c.holder.name)
	}
	b := &fakeBranch{holder: c.holder, handle: &fakeHandle{generation: c.generation, branch: string(rune('a' + c.holder.branches - 1))}}
	if c.holder.branchErr != nil {
		return b, c.holder.branchErr
	}
	return b, nil
}

type fakeBranch struct {
	holder *fakeHolder
	handle any
}

func (b *fakeBranch) Handle() any { return b.handle }
func (b *fakeBranch) Finish(context.Context) error {
	b.holder.finishes++
	if b.holder.log != nil {
		*b.holder.log = append(*b.holder.log, "finish:"+b.holder.name)
	}
	return b.holder.finishErr
}

func mustRegistry(t *testing.T, holders ...*fakeHolder) *Registry {
	t.Helper()
	registrations := make([]Registration, len(holders))
	for i, holder := range holders {
		registrations[i] = Registration{Name: holder.name, Holder: holder}
	}
	registry, err := NewRegistry(registrations...)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestRegistryRejectsEmptyAndDuplicateNames(t *testing.T) {
	holder := &fakeHolder{name: "primary"}
	if _, err := NewRegistry(Registration{Name: " ", Holder: holder}); !errors.Is(err, ErrEmptyHolderName) {
		t.Fatalf("empty name error = %v", err)
	}
	if _, err := NewRegistry(Registration{Name: "db", Holder: holder}, Registration{Name: " db ", Holder: holder}); !errors.Is(err, ErrDuplicateHolderName) {
		t.Fatalf("duplicate name error = %v", err)
	}
	if holder.captures != 0 {
		t.Fatalf("validation invoked holder %d times", holder.captures)
	}
}

func TestCapturePublishesOnlyCompleteGroups(t *testing.T) {
	wantErr := errors.New("capture failed")
	first := &fakeHolder{name: "primary"}
	second := &fakeHolder{name: "audit", captureErr: wantErr}
	checkpoint, err := mustRegistry(t, first, second).Capture(context.Background(), CheckpointMeta{ID: "journey/checkpoint"})
	if checkpoint != nil {
		t.Fatal("partial checkpoint was published")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("capture error = %v", err)
	}
	if first.releases != 1 || second.releases != 1 {
		t.Fatalf("release counts = %d, %d", first.releases, second.releases)
	}
}

func TestCaptureFailureCompensatesInReverseOrder(t *testing.T) {
	var log []string
	holders := []*fakeHolder{{name: "one", log: &log}, {name: "two", log: &log}, {name: "three", log: &log, captureErr: errors.New("boom")}}
	_, _ = mustRegistry(t, holders...).Capture(context.Background(), CheckpointMeta{ID: "cp"})
	want := []string{"capture:one", "capture:two", "capture:three", "release:three", "release:two", "release:one"}
	if !reflect.DeepEqual(log, want) {
		t.Fatalf("lifecycle order = %v, want %v", log, want)
	}
}

func TestBranchFailureDoesNotInvokeApplicationFactory(t *testing.T) {
	first := &fakeHolder{name: "primary"}
	second := &fakeHolder{name: "audit", branchErr: errors.New("branch failed")}
	cp, err := mustRegistry(t, first, second).Capture(context.Background(), CheckpointMeta{ID: "cp"})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	branch, err := cp.StartBranch(context.Background(), "child", func(context.Context, Handles) (any, error) { called = true; return struct{}{}, nil })
	if branch != nil || err == nil {
		t.Fatalf("StartBranch() = %v, %v", branch, err)
	}
	if called {
		t.Fatal("factory ran for partial replacement group")
	}
}

func TestBranchFailureCompensatesInReverseOrder(t *testing.T) {
	var log []string
	holders := []*fakeHolder{{name: "one", log: &log}, {name: "two", log: &log}, {name: "three", log: &log, branchErr: errors.New("boom")}}
	cp, err := mustRegistry(t, holders...).Capture(context.Background(), CheckpointMeta{ID: "cp"})
	if err != nil {
		t.Fatal(err)
	}
	log = nil
	_, _ = cp.StartBranch(context.Background(), "child", func(context.Context, Handles) (any, error) { return struct{}{}, nil })
	want := []string{"branch:one", "branch:two", "branch:three", "finish:three", "finish:two", "finish:one"}
	if !reflect.DeepEqual(log, want) {
		t.Fatalf("lifecycle order = %v, want %v", log, want)
	}
}

func TestCleanupFailuresAreQuarantined(t *testing.T) {
	wantCleanup := errors.New("cleanup failed")
	first := &fakeHolder{name: "primary", releaseErr: wantCleanup}
	second := &fakeHolder{name: "audit", captureErr: errors.New("capture failed")}
	_, err := mustRegistry(t, first, second).Capture(context.Background(), CheckpointMeta{ID: "cp"})
	var lifecycleErr *LifecycleError
	if !errors.As(err, &lifecycleErr) {
		t.Fatalf("error type = %T", err)
	}
	if len(lifecycleErr.Quarantined) != 1 || lifecycleErr.Quarantined[0].Holder != "primary" || !errors.Is(lifecycleErr.Quarantined[0].Err, wantCleanup) {
		t.Fatalf("quarantined = %+v", lifecycleErr.Quarantined)
	}
}

func TestReleaseAndFinishAreIdempotent(t *testing.T) {
	holder := &fakeHolder{name: "primary"}
	cp, err := mustRegistry(t, holder).Capture(context.Background(), CheckpointMeta{ID: "cp"})
	if err != nil {
		t.Fatal(err)
	}
	branch, err := cp.StartBranch(context.Background(), "child", func(context.Context, Handles) (any, error) { return struct{}{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = branch.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = branch.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = cp.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = cp.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if holder.finishes != 1 || holder.releases != 1 {
		t.Fatalf("finish/release counts = %d/%d", holder.finishes, holder.releases)
	}
	if got := cp.Evidence().Cleanup.Status; got != "released" {
		t.Fatalf("checkpoint cleanup status = %q", got)
	}
	if got := branch.Evidence().Cleanup.Status; got != "released" {
		t.Fatalf("branch cleanup status = %q", got)
	}
}

func TestReleaseReturnsStableCleanupFailure(t *testing.T) {
	wantErr := errors.New("release failed")
	holder := &fakeHolder{name: "primary", releaseErr: wantErr}
	cp, err := mustRegistry(t, holder).Capture(context.Background(), CheckpointMeta{ID: "cp"})
	if err != nil {
		t.Fatal(err)
	}
	first := cp.Release(context.Background())
	second := cp.Release(context.Background())
	if !errors.Is(first, wantErr) || !errors.Is(second, wantErr) {
		t.Fatalf("release errors = %v, %v", first, second)
	}
	if holder.releases != 1 {
		t.Fatalf("release count = %d", holder.releases)
	}
	evidence := cp.Evidence()
	if evidence.Cleanup.Status != "quarantined" || len(evidence.Cleanup.Quarantined) != 1 {
		t.Fatalf("cleanup evidence = %+v", evidence.Cleanup)
	}
}

func TestEvidenceIsExplicitlyDatabaseOnly(t *testing.T) {
	holder := &fakeHolder{name: "primary"}
	cp, err := mustRegistry(t, holder).Capture(context.Background(), CheckpointMeta{ID: "new/list/few-items-added", ParentID: "new/onboarding-complete", Source: "listus.go", Revision: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	evidence := cp.Evidence()
	if evidence.Scope != ScopeDatabaseOnly || evidence.Mechanism != MechanismBranch {
		t.Fatalf("scope/mechanism = %q/%q", evidence.Scope, evidence.Mechanism)
	}
	if evidence.CheckpointID != "new/list/few-items-added" || evidence.ParentCheckpointID != "new/onboarding-complete" {
		t.Fatalf("lineage = %+v", evidence)
	}
	wantExcluded := []string{"chatwright-emulator", "clock", "queue", "process", "cache", "filesystem"}
	for _, want := range wantExcluded {
		found := false
		for _, got := range evidence.ExcludedState {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("excluded state does not contain %q: %v", want, evidence.ExcludedState)
		}
	}
}

func TestBranchFactoryReceivesOnlyFreshReplacementHandles(t *testing.T) {
	source := &fakeHandle{generation: "live", branch: "source"}
	holder := &fakeHolder{name: "primary", source: source}
	cp, err := mustRegistry(t, holder).Capture(context.Background(), CheckpointMeta{ID: "cp"})
	if err != nil {
		t.Fatal(err)
	}
	var first, second *fakeHandle
	factory := func(_ context.Context, handles Handles) (any, error) {
		handle := handles["primary"].(*fakeHandle)
		if handle == source {
			t.Fatal("factory received live source handle")
		}
		return handle, nil
	}
	b1, err := cp.StartBranch(context.Background(), "one", factory)
	if err != nil {
		t.Fatal(err)
	}
	first = b1.Environment().(*fakeHandle)
	if err = b1.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	b2, err := cp.StartBranch(context.Background(), "two", factory)
	if err != nil {
		t.Fatal(err)
	}
	second = b2.Environment().(*fakeHandle)
	if first == second {
		t.Fatal("siblings received the same replacement handle")
	}
}

func TestRunSequentialFinishesBeforeStartingNext(t *testing.T) {
	var log []string
	holder := &fakeHolder{name: "primary", log: &log}
	cp, err := mustRegistry(t, holder).Capture(context.Background(), CheckpointMeta{ID: "cp"})
	if err != nil {
		t.Fatal(err)
	}
	log = nil
	results := cp.RunSequential(context.Background(),
		BranchSpec{Name: "one", Factory: func(context.Context, Handles) (any, error) { log = append(log, "factory:one"); return struct{}{}, nil }, Continue: func(context.Context, any, Evidence) error { log = append(log, "run:one"); return nil }},
		BranchSpec{Name: "two", Factory: func(context.Context, Handles) (any, error) { log = append(log, "factory:two"); return struct{}{}, nil }},
	)
	if len(results) != 2 || results[0].Err != nil || results[1].Err != nil {
		t.Fatalf("results = %+v", results)
	}
	want := []string{"branch:primary", "factory:one", "run:one", "finish:primary", "branch:primary", "factory:two", "finish:primary"}
	if !reflect.DeepEqual(log, want) {
		t.Fatalf("order = %v, want %v", log, want)
	}
}

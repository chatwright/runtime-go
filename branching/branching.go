// Package branching coordinates database-only scenario checkpoints and branches.
package branching

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Scope is the state boundary covered by checkpoint and branch evidence.
type Scope string

const (
	// ScopeDatabaseOnly means only registered database holders are isolated.
	ScopeDatabaseOnly Scope = "database-only"

	// MechanismBranch identifies a native branch created from a checkpoint.
	MechanismBranch = "branch"
)

var (
	ErrEmptyHolderName       = errors.New("branching: holder name is empty")
	ErrDuplicateHolderName   = errors.New("branching: duplicate holder name")
	ErrNilHolder             = errors.New("branching: holder is nil")
	ErrNilCheckpoint         = errors.New("branching: holder returned a nil checkpoint")
	ErrEmptyGeneration       = errors.New("branching: holder checkpoint generation is empty")
	ErrNilBranch             = errors.New("branching: holder returned a nil branch")
	ErrNilReplacementHandle  = errors.New("branching: holder returned a nil replacement handle")
	ErrReleasedCheckpoint    = errors.New("branching: checkpoint is released")
	ErrEmptyCheckpointID     = errors.New("branching: checkpoint identity is empty")
	ErrEmptyBranchName       = errors.New("branching: branch name is empty")
	ErrDuplicateBranchName   = errors.New("branching: duplicate branch name")
	ErrNilEnvironmentFactory = errors.New("branching: environment factory is nil")
)

// DefaultExcludedState is copied into every evidence record. It prevents a
// database checkpoint from being presented as a complete process snapshot.
var DefaultExcludedState = []string{
	"chatwright-emulator",
	"message-handles",
	"message-consumption-cursors",
	"clock",
	"queue",
	"process",
	"cache",
	"filesystem",
}

// Capability describes the implementation behind one registered holder.
type Capability struct {
	Provider string
	Version  string
}

// Holder creates immutable checkpoints of one application-owned database.
type Holder interface {
	Capability() Capability
	Capture(context.Context) (HolderCheckpoint, error)
}

// HolderCheckpoint is the provider-owned state captured for one holder.
type HolderCheckpoint interface {
	Generation() string
	Branch(context.Context) (HolderBranch, error)
	Release(context.Context) error
}

// HolderBranch owns one fresh replacement database handle.
type HolderBranch interface {
	Handle() any
	Finish(context.Context) error
}

// Registration assigns the application-level identity used in evidence and errors.
type Registration struct {
	Name   string
	Holder Holder
}

// Registry is an immutable, validated holder registration order.
type Registry struct {
	registrations []Registration
}

// NewRegistry validates all names before any holder lifecycle method can run.
func NewRegistry(registrations ...Registration) (*Registry, error) {
	seen := make(map[string]struct{}, len(registrations))
	validated := make([]Registration, len(registrations))
	for i, registration := range registrations {
		name := strings.TrimSpace(registration.Name)
		if name == "" {
			return nil, fmt.Errorf("%w at registration %d", ErrEmptyHolderName, i)
		}
		if registration.Holder == nil {
			return nil, fmt.Errorf("%w: %s", ErrNilHolder, name)
		}
		if _, ok := seen[name]; ok {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateHolderName, name)
		}
		seen[name] = struct{}{}
		validated[i] = Registration{Name: name, Holder: registration.Holder}
	}
	return &Registry{registrations: validated}, nil
}

// CheckpointMeta supplies the semantic identity and lineage owned by a scenario.
type CheckpointMeta struct {
	ID       string
	ParentID string
	Source   string
	Revision string
}

// HolderEvidence identifies one captured database generation.
type HolderEvidence struct {
	Name       string
	Provider   string
	Version    string
	Generation string
}

// CleanupEvidence records whether lifecycle resources were released cleanly.
type CleanupEvidence struct {
	Status      string
	Quarantined []QuarantinedResource
}

// Evidence states exactly what a checkpoint or branch isolated.
type Evidence struct {
	Scope              Scope
	CheckpointID       string
	ParentCheckpointID string
	Source             string
	Revision           string
	BranchName         string
	Mechanism          string
	Holders            []HolderEvidence
	ExcludedState      []string
	Cleanup            CleanupEvidence
}

// QuarantinedResource is a partial resource whose compensation failed.
type QuarantinedResource struct {
	Holder string
	Phase  string
	Err    error
}

// LifecycleError retains the primary lifecycle failure and any failed cleanup.
type LifecycleError struct {
	Phase       string
	Holder      string
	Cause       error
	Quarantined []QuarantinedResource
}

func (e *LifecycleError) Error() string {
	if len(e.Quarantined) == 0 {
		return fmt.Sprintf("branching: %s failed for holder %q: %v", e.Phase, e.Holder, e.Cause)
	}
	return fmt.Sprintf("branching: %s failed for holder %q: %v (%d resource(s) quarantined)", e.Phase, e.Holder, e.Cause, len(e.Quarantined))
}

// Unwrap preserves the primary failure for errors.Is and errors.As.
func (e *LifecycleError) Unwrap() error { return e.Cause }

type capturedHolder struct {
	name       string
	capability Capability
	checkpoint HolderCheckpoint
	generation string

	releaseOnce sync.Once
	releaseErr  error
}

func (h *capturedHolder) release(ctx context.Context) error {
	h.releaseOnce.Do(func() { h.releaseErr = h.checkpoint.Release(ctx) })
	return h.releaseErr
}

// Checkpoint is an all-holders immutable checkpoint.
type Checkpoint struct {
	meta    CheckpointMeta
	holders []*capturedHolder

	mu          sync.Mutex
	released    bool
	branches    map[string]struct{}
	releaseOnce sync.Once
	releaseErr  error
	cleanup     CleanupEvidence
}

// Capture creates one publishable checkpoint only after all holders succeed.
func (r *Registry) Capture(ctx context.Context, meta CheckpointMeta) (*Checkpoint, error) {
	meta.ID = strings.TrimSpace(meta.ID)
	if meta.ID == "" {
		return nil, ErrEmptyCheckpointID
	}
	captured := make([]*capturedHolder, 0, len(r.registrations))
	for _, registration := range r.registrations {
		checkpoint, err := registration.Holder.Capture(ctx)
		if checkpoint != nil {
			captured = append(captured, &capturedHolder{
				name:       registration.Name,
				capability: registration.Holder.Capability(),
				checkpoint: checkpoint,
				generation: checkpoint.Generation(),
			})
		}
		if err != nil {
			quarantined := releaseCapturedReverse(ctx, captured, "capture-compensation")
			return nil, &LifecycleError{Phase: "capture", Holder: registration.Name, Cause: err, Quarantined: quarantined}
		}
		if checkpoint == nil {
			quarantined := releaseCapturedReverse(ctx, captured, "capture-compensation")
			return nil, &LifecycleError{Phase: "capture", Holder: registration.Name, Cause: ErrNilCheckpoint, Quarantined: quarantined}
		}
		if strings.TrimSpace(checkpoint.Generation()) == "" {
			quarantined := releaseCapturedReverse(ctx, captured, "capture-compensation")
			return nil, &LifecycleError{Phase: "capture", Holder: registration.Name, Cause: ErrEmptyGeneration, Quarantined: quarantined}
		}
	}
	return &Checkpoint{
		meta:     meta,
		holders:  captured,
		branches: make(map[string]struct{}),
		cleanup:  CleanupEvidence{Status: "pending"},
	}, nil
}

// Evidence returns a defensive snapshot of checkpoint evidence.
func (c *Checkpoint) Evidence() Evidence {
	c.mu.Lock()
	defer c.mu.Unlock()
	evidence := evidenceFor(c.meta, "", c.holders)
	evidence.Cleanup = CleanupEvidence{
		Status:      c.cleanup.Status,
		Quarantined: append([]QuarantinedResource(nil), c.cleanup.Quarantined...),
	}
	return evidence
}

// Release releases every holder checkpoint in reverse registration order.
// Repeated calls return the same cleanup result without calling providers again.
func (c *Checkpoint) Release(ctx context.Context) error {
	c.releaseOnce.Do(func() {
		c.mu.Lock()
		c.released = true
		c.mu.Unlock()
		quarantined := releaseCapturedReverse(ctx, c.holders, "checkpoint-release")
		c.mu.Lock()
		defer c.mu.Unlock()
		if len(quarantined) == 0 {
			c.cleanup.Status = "released"
			return
		}
		c.cleanup.Status = "quarantined"
		c.cleanup.Quarantined = append([]QuarantinedResource(nil), quarantined...)
		c.releaseErr = cleanupError("checkpoint-release", quarantined)
	})
	return c.releaseErr
}

// Handles maps application names to fresh provider replacement handles.
type Handles map[string]any

// EnvironmentFactory binds a complete replacement group into a fresh app world.
type EnvironmentFactory func(context.Context, Handles) (any, error)

type startedHolder struct {
	name   string
	branch HolderBranch

	finishOnce sync.Once
	finishErr  error
}

func (h *startedHolder) finish(ctx context.Context) error {
	h.finishOnce.Do(func() { h.finishErr = h.branch.Finish(ctx) })
	return h.finishErr
}

// Branch is one fully bound sibling environment.
type Branch struct {
	checkpoint  *Checkpoint
	name        string
	started     []*startedHolder
	handles     Handles
	environment any

	finishOnce sync.Once
	finishErr  error
	evidenceMu sync.Mutex
	evidence   Evidence
}

// StartBranch creates all replacements before invoking the application factory.
func (c *Checkpoint) StartBranch(ctx context.Context, name string, factory EnvironmentFactory) (*Branch, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ErrEmptyBranchName
	}
	if factory == nil {
		return nil, ErrNilEnvironmentFactory
	}
	c.mu.Lock()
	if c.released {
		c.mu.Unlock()
		return nil, ErrReleasedCheckpoint
	}
	if _, exists := c.branches[name]; exists {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrDuplicateBranchName, name)
	}
	c.mu.Unlock()

	started := make([]*startedHolder, 0, len(c.holders))
	handles := make(Handles, len(c.holders))
	for _, captured := range c.holders {
		replacement, err := captured.checkpoint.Branch(ctx)
		if replacement != nil {
			started = append(started, &startedHolder{name: captured.name, branch: replacement})
		}
		if err != nil {
			quarantined := finishStartedReverse(ctx, started, "branch-compensation")
			return nil, &LifecycleError{Phase: "branch", Holder: captured.name, Cause: err, Quarantined: quarantined}
		}
		if replacement == nil {
			quarantined := finishStartedReverse(ctx, started, "branch-compensation")
			return nil, &LifecycleError{Phase: "branch", Holder: captured.name, Cause: ErrNilBranch, Quarantined: quarantined}
		}
		handle := replacement.Handle()
		if handle == nil {
			quarantined := finishStartedReverse(ctx, started, "branch-compensation")
			return nil, &LifecycleError{Phase: "branch", Holder: captured.name, Cause: ErrNilReplacementHandle, Quarantined: quarantined}
		}
		handles[captured.name] = handle
	}

	environment, err := factory(ctx, cloneHandles(handles))
	if err != nil {
		quarantined := finishStartedReverse(ctx, started, "factory-compensation")
		return nil, &LifecycleError{Phase: "bind-application", Holder: "", Cause: err, Quarantined: quarantined}
	}
	c.mu.Lock()
	if c.released {
		c.mu.Unlock()
		quarantined := finishStartedReverse(ctx, started, "branch-compensation")
		return nil, &LifecycleError{Phase: "branch", Holder: "", Cause: ErrReleasedCheckpoint, Quarantined: quarantined}
	}
	if _, exists := c.branches[name]; exists {
		c.mu.Unlock()
		quarantined := finishStartedReverse(ctx, started, "branch-compensation")
		return nil, &LifecycleError{Phase: "branch", Holder: "", Cause: fmt.Errorf("%w: %s", ErrDuplicateBranchName, name), Quarantined: quarantined}
	}
	c.branches[name] = struct{}{}
	c.mu.Unlock()

	evidence := evidenceFor(c.meta, name, c.holders)
	evidence.Cleanup.Status = "pending"
	return &Branch{
		checkpoint:  c,
		name:        name,
		started:     started,
		handles:     cloneHandles(handles),
		environment: environment,
		evidence:    evidence,
	}, nil
}

// Environment returns the freshly bound application environment.
func (b *Branch) Environment() any { return b.environment }

// Handles returns a defensive copy of the replacements bound for this branch.
func (b *Branch) Handles() Handles { return cloneHandles(b.handles) }

// Evidence returns a defensive copy of branch evidence.
func (b *Branch) Evidence() Evidence {
	b.evidenceMu.Lock()
	defer b.evidenceMu.Unlock()
	return cloneEvidence(b.evidence)
}

// Finish cleans the replacement group in reverse order and is idempotent.
func (b *Branch) Finish(ctx context.Context) error {
	b.finishOnce.Do(func() {
		quarantined := finishStartedReverse(ctx, b.started, "branch-finish")
		b.evidenceMu.Lock()
		if len(quarantined) == 0 {
			b.evidence.Cleanup.Status = "released"
		} else {
			b.evidence.Cleanup.Status = "quarantined"
			b.evidence.Cleanup.Quarantined = append([]QuarantinedResource(nil), quarantined...)
			b.finishErr = cleanupError("branch-finish", quarantined)
		}
		b.evidenceMu.Unlock()
	})
	return b.finishErr
}

// BranchSpec is one sibling executed by RunSequential.
type BranchSpec struct {
	Name     string
	Factory  EnvironmentFactory
	Continue func(context.Context, any, Evidence) error
}

// BranchResult records continuation and cleanup for one sibling.
type BranchResult struct {
	Name     string
	Evidence Evidence
	Err      error
}

// RunSequential starts, runs and finishes each sibling before starting the next.
func (c *Checkpoint) RunSequential(ctx context.Context, specs ...BranchSpec) []BranchResult {
	results := make([]BranchResult, 0, len(specs))
	for _, spec := range specs {
		branch, err := c.StartBranch(ctx, spec.Name, spec.Factory)
		if err != nil {
			results = append(results, BranchResult{Name: spec.Name, Err: err})
			continue
		}
		if spec.Continue != nil {
			err = spec.Continue(ctx, branch.Environment(), branch.Evidence())
		}
		finishErr := branch.Finish(ctx)
		results = append(results, BranchResult{
			Name:     spec.Name,
			Evidence: branch.Evidence(),
			Err:      errors.Join(err, finishErr),
		})
	}
	return results
}

func evidenceFor(meta CheckpointMeta, branchName string, holders []*capturedHolder) Evidence {
	evidence := Evidence{
		Scope:              ScopeDatabaseOnly,
		CheckpointID:       meta.ID,
		ParentCheckpointID: meta.ParentID,
		Source:             meta.Source,
		Revision:           meta.Revision,
		BranchName:         branchName,
		Mechanism:          MechanismBranch,
		Holders:            make([]HolderEvidence, len(holders)),
		ExcludedState:      append([]string(nil), DefaultExcludedState...),
		Cleanup:            CleanupEvidence{Status: "pending"},
	}
	for i, holder := range holders {
		evidence.Holders[i] = HolderEvidence{
			Name:       holder.name,
			Provider:   holder.capability.Provider,
			Version:    holder.capability.Version,
			Generation: holder.generation,
		}
	}
	return evidence
}

func releaseCapturedReverse(ctx context.Context, captured []*capturedHolder, phase string) []QuarantinedResource {
	var quarantined []QuarantinedResource
	for i := len(captured) - 1; i >= 0; i-- {
		if err := captured[i].release(ctx); err != nil {
			quarantined = append(quarantined, QuarantinedResource{Holder: captured[i].name, Phase: phase, Err: err})
		}
	}
	return quarantined
}

func finishStartedReverse(ctx context.Context, started []*startedHolder, phase string) []QuarantinedResource {
	var quarantined []QuarantinedResource
	for i := len(started) - 1; i >= 0; i-- {
		if err := started[i].finish(ctx); err != nil {
			quarantined = append(quarantined, QuarantinedResource{Holder: started[i].name, Phase: phase, Err: err})
		}
	}
	return quarantined
}

func cleanupError(phase string, quarantined []QuarantinedResource) error {
	errs := make([]error, 0, len(quarantined))
	for _, resource := range quarantined {
		errs = append(errs, fmt.Errorf("%s/%s: %w", resource.Holder, resource.Phase, resource.Err))
	}
	return fmt.Errorf("branching: %s cleanup failed: %w", phase, errors.Join(errs...))
}

func cloneHandles(handles Handles) Handles {
	cloned := make(Handles, len(handles))
	for name, handle := range handles {
		cloned[name] = handle
	}
	return cloned
}

func cloneEvidence(evidence Evidence) Evidence {
	evidence.Holders = append([]HolderEvidence(nil), evidence.Holders...)
	evidence.ExcludedState = append([]string(nil), evidence.ExcludedState...)
	evidence.Cleanup.Quarantined = append([]QuarantinedResource(nil), evidence.Cleanup.Quarantined...)
	return evidence
}

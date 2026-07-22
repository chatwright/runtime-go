package chatwright

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
)

// SourceReference links scenario evidence to the definition that produced it.
// URI should identify the source location (usually a VCS blob URL), while
// Revision identifies the exact version that was executed.
type SourceReference struct {
	URI      string
	Revision string
}

// Definition identifies a scenario or reusable fragment and its source.
type Definition struct {
	Name   string
	Source SourceReference
}

// InputSource describes where one named effective input came from. Kind is an
// application-defined category such as "fixture", "default", or "override".
type InputSource struct {
	Kind      string
	Reference string
	Source    SourceReference
}

// EffectiveInputs holds a fragment's typed input value and the provenance of
// its named fields. Sources is copied for each invocation.
type EffectiveInputs[T any] struct {
	Value   T
	Sources map[string]InputSource
}

// InvocationPath is the qualified machine path of a scenario or fragment
// invocation. Paths are built from escaped, non-empty segments.
type InvocationPath string

func (p InvocationPath) String() string { return string(p) }

func invocationPath(segments ...string) (InvocationPath, error) {
	if len(segments) == 0 {
		return "", errors.New("chatwright: invocation path requires at least one segment")
	}
	escaped := make([]string, len(segments))
	for i, segment := range segments {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			return "", fmt.Errorf("chatwright: invocation path segment %d is empty", i)
		}
		escaped[i] = url.PathEscape(segment)
	}
	return InvocationPath(strings.Join(escaped, "/")), nil
}

func (p InvocationPath) child(segment string) (InvocationPath, error) {
	child, err := invocationPath(segment)
	if err != nil {
		return "", err
	}
	if p == "" {
		return child, nil
	}
	return InvocationPath(string(p) + "/" + string(child)), nil
}

// CheckpointID is a checkpoint's qualified machine identity.
type CheckpointID string

// StepEvidence records a step produced directly by one invocation.
type StepEvidence struct {
	Name           string
	InvocationPath InvocationPath
	Source         SourceReference
}

// BranchEvidence records a branch declared directly by one invocation.
type BranchEvidence struct {
	Name           string
	InvocationPath InvocationPath
	Source         SourceReference
}

// FailureEvidence records a failure attributed to one invocation and source.
type FailureEvidence struct {
	Message        string
	InvocationPath InvocationPath
	Source         SourceReference
}

// CheckpointEvidence records a named checkpoint and its qualified lineage.
// Lineage contains ancestor checkpoint IDs in root-to-parent order.
type CheckpointEvidence struct {
	ID             CheckpointID
	Name           string
	InvocationPath InvocationPath
	ParentID       CheckpointID
	Lineage        []CheckpointID
	Source         SourceReference
}

// ExecutionEvidence is a snapshot of evidence produced directly by an
// execution context. Evidence from nested fragment invocations remains attached
// to those invocations rather than being flattened into their caller.
type ExecutionEvidence struct {
	Path        InvocationPath
	Definition  Definition
	Steps       []StepEvidence
	Checkpoints []CheckpointEvidence
	Branches    []BranchEvidence
	Failures    []FailureEvidence
}

// ExecutionContext is the small, invocation-local context used by scenarios
// and fragments to record source-linked evidence and checkpoint lineage.
type ExecutionContext struct {
	mu sync.Mutex

	path       InvocationPath
	definition Definition

	steps       []StepEvidence
	checkpoints []CheckpointEvidence
	branches    []BranchEvidence
	failures    []FailureEvidence
	current     *CheckpointEvidence
}

// NewExecutionContext creates a root scenario execution. Each path argument is
// one machine-path segment, for example "listus" and "new-user".
func NewExecutionContext(definition Definition, path ...string) (*ExecutionContext, error) {
	if strings.TrimSpace(definition.Name) == "" {
		return nil, errors.New("chatwright: scenario definition name is empty")
	}
	qualifiedPath, err := invocationPath(path...)
	if err != nil {
		return nil, err
	}
	return &ExecutionContext{path: qualifiedPath, definition: definition}, nil
}

// Path returns the qualified path for this execution context.
func (c *ExecutionContext) Path() InvocationPath {
	if c == nil {
		return ""
	}
	return c.path
}

// RecordStep adds source-linked evidence for a locally produced step.
func (c *ExecutionContext) RecordStep(name string, source SourceReference) StepEvidence {
	evidence := StepEvidence{Name: name, InvocationPath: c.path, Source: source}
	c.mu.Lock()
	c.steps = append(c.steps, evidence)
	c.mu.Unlock()
	return evidence
}

// RecordBranch adds source-linked evidence for a locally declared branch.
func (c *ExecutionContext) RecordBranch(name string, source SourceReference) BranchEvidence {
	evidence := BranchEvidence{Name: name, InvocationPath: c.path, Source: source}
	c.mu.Lock()
	c.branches = append(c.branches, evidence)
	c.mu.Unlock()
	return evidence
}

// RecordFailure adds source-linked evidence for a locally observed failure.
func (c *ExecutionContext) RecordFailure(err error, source SourceReference) FailureEvidence {
	message := ""
	if err != nil {
		message = err.Error()
	}
	evidence := FailureEvidence{Message: message, InvocationPath: c.path, Source: source}
	c.mu.Lock()
	c.failures = append(c.failures, evidence)
	c.mu.Unlock()
	return evidence
}

// Checkpoint records a named checkpoint qualified by the current invocation
// path. The previously active checkpoint, including one inherited from a parent
// invocation, becomes its parent and is appended to its lineage.
func (c *ExecutionContext) Checkpoint(name string, source SourceReference) (CheckpointEvidence, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return CheckpointEvidence{}, errors.New("chatwright: checkpoint name is empty")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	checkpoint := CheckpointEvidence{
		ID:             CheckpointID(string(c.path) + "/checkpoints/" + url.PathEscape(name)),
		Name:           name,
		InvocationPath: c.path,
		Source:         source,
	}
	if c.current != nil {
		checkpoint.ParentID = c.current.ID
		checkpoint.Lineage = append(cloneCheckpointIDs(c.current.Lineage), c.current.ID)
	}
	c.checkpoints = append(c.checkpoints, checkpoint)
	current := cloneCheckpoint(checkpoint)
	c.current = &current
	return cloneCheckpoint(checkpoint), nil
}

// Evidence returns a detached snapshot of the evidence produced directly by
// this execution context.
func (c *ExecutionContext) Evidence() ExecutionEvidence {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ExecutionEvidence{
		Path:        c.path,
		Definition:  c.definition,
		Steps:       append([]StepEvidence(nil), c.steps...),
		Checkpoints: cloneCheckpoints(c.checkpoints),
		Branches:    append([]BranchEvidence(nil), c.branches...),
		Failures:    append([]FailureEvidence(nil), c.failures...),
	}
}

func (c *ExecutionContext) inheritedCheckpoint() *CheckpointEvidence {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current == nil {
		return nil
	}
	checkpoint := cloneCheckpoint(*c.current)
	return &checkpoint
}

func (c *ExecutionContext) adoptCheckpoint(checkpoint *CheckpointEvidence) {
	if checkpoint == nil {
		return
	}
	c.mu.Lock()
	adopted := cloneCheckpoint(*checkpoint)
	c.current = &adopted
	c.mu.Unlock()
}

func cloneCheckpoint(checkpoint CheckpointEvidence) CheckpointEvidence {
	checkpoint.Lineage = cloneCheckpointIDs(checkpoint.Lineage)
	return checkpoint
}

func cloneCheckpoints(checkpoints []CheckpointEvidence) []CheckpointEvidence {
	cloned := make([]CheckpointEvidence, len(checkpoints))
	for i, checkpoint := range checkpoints {
		cloned[i] = cloneCheckpoint(checkpoint)
	}
	return cloned
}

func cloneCheckpointIDs(ids []CheckpointID) []CheckpointID {
	return append([]CheckpointID(nil), ids...)
}

func cloneInputSources(sources map[string]InputSource) map[string]InputSource {
	if sources == nil {
		return nil
	}
	cloned := make(map[string]InputSource, len(sources))
	for name, source := range sources {
		cloned[name] = source
	}
	return cloned
}

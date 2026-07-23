// Package datastate is the smallest provider-neutral data-state assertion
// runtime for the data-state-assertions feature
// (spec/features/chatwright/deterministic-testing/data-state-assertions/README.md):
// run a read-only DTQL query against a named application database after a
// settled message/action, immediately before a checkpoint is published, or
// at branch/fragment completion, and retain a bounded, redacted recordset as
// evidence so a scenario proves what the application stored, not only what
// the bot said.
//
// This package does not parse, validate or execute DTQL itself. The
// Executor interface is the seam: it takes the concrete DTQL text plus
// named parameters and returns rows. DALgo's real DTQL/dal.DB executor
// (Task 4 of spec/plans/listus-branching-reference-scenario.md) satisfies
// it; this package's own tests use a fake. datastate never invents a
// private query language and never falls back to scanning records itself
// when an Executor reports an error.
//
// datastate is deliberately independent of the branching package: it knows
// nothing about Registry, Checkpoint or Branch. A scenario wires the two
// together itself — typically by converting a branching.Branch's Handles
// (also a map[string]any) into datastate.Handles, and by passing the
// checkpoint's actual publish step (e.g. registry.Capture) as Gate's commit
// callback. See Gate for how a checkpoint publication is made to require
// named assertions to pass first without datastate importing branching's
// types, and Handles for why the map[string]any conversion is direct.
package datastate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// AttachmentPoint names where in a scenario a data assertion runs, matching
// the data-state-assertions feature's "Assertion attachment points"
// behaviour: after a user message or action's registered application work
// has settled, immediately before a named checkpoint is published, or at
// the end of a branch or scenario fragment.
type AttachmentPoint string

const (
	// AttachmentAfterMessage is a data assertion attached after a message or
	// action and its registered application work has settled.
	AttachmentAfterMessage AttachmentPoint = "after-message"
	// AttachmentCheckpoint is a data assertion that gates a named
	// checkpoint's publication.
	AttachmentCheckpoint AttachmentPoint = "checkpoint"
	// AttachmentBranchCompletion is a data assertion run at the end of a
	// branch or scenario fragment.
	AttachmentBranchCompletion AttachmentPoint = "branch-completion"
)

// Outcome is the pass/fail result of one executed assertion.
type Outcome string

const (
	OutcomePassed Outcome = "passed"
	OutcomeFailed Outcome = "failed"
)

// Row is one returned record. Field values may themselves be nested
// map[string]any or []any, matching an embedded-document shape such as
// Listus's parent-scoped list record and its nested `items` field.
type Row map[string]any

// Query is the canonical DTQL artifact executed by an assertion: the exact
// DTQL text plus its named parameters. It is stored verbatim in evidence so
// a compatible DataTug surface opens the same query Chatwright executed
// (data-state-assertions#ac:dtql-is-the-shared-artifact) — datastate never
// translates it through a private representation. The first release does
// not add placeholder syntax to DTQL: the text stored here already contains
// resolved values, and Params is carried alongside only because a real
// dal.StructuredQuery executor may still want named parameters bound
// separately from the query text.
type Query struct {
	DTQL   string
	Params map[string]any
}

// Executor runs one DTQL query against a resolved database handle and
// returns its rows. The real implementation is DALgo's dtql package plus a
// query-capable dal.DB (Task 4); this package depends only on the
// interface, and its own contract tests use a fake. An Executor must fail
// explicitly for unsupported query/provider capabilities rather than
// silently scanning all records or falling back to application code.
type Executor interface {
	Execute(ctx context.Context, handle any, query Query) ([]Row, error)
}

// Handles names the registered database handles an assertion may run
// against, keyed by the same application-chosen holder name used at
// registration (see branching.Registration.Name). It is intentionally
// identical in shape to branching.Handles (map[string]any) without
// importing the branching package: converting one to the other is a direct
// Go type conversion, e.g. datastate.Handles(branch.Handles()). A Runner
// must be built from the handles bound to the current environment — inside
// a branch that is always the branch's own replacement handles, never the
// source or a sibling's.
type Handles map[string]any

// Normalization declares how rows are canonicalised before an assertion's
// Expect runs and before a preview is built: the field names excluded from
// that comparison (typically generated IDs or timestamps) and an optional
// explicit sort key. Excluding a field only removes it from the comparison
// basis — it is still copied into the evidence preview, so normalisation
// can never make a field invisible, only irrelevant to the assertion.
//
// Rows are always ordered deterministically. When SortKeys is set, rows are
// ordered by those field values; otherwise they are ordered by each row's
// canonical JSON encoding (Go's encoding/json sorts object keys), so two
// runs over the same underlying data always render evidence in the same
// order even without an author-declared sort key.
type Normalization struct {
	SortKeys       []string
	ExcludedFields []string
}

// Limits bounds how much of a result set is copied into evidence: at most
// MaxRows rows, and at most MaxFields fields per row (fields are kept in
// alphabetical order so truncation is deterministic). Zero means "use
// DefaultLimits" — this package always bounds a preview, per the
// data-state-assertions feature's bounded/redacted evidence requirement.
type Limits struct {
	MaxRows   int
	MaxFields int
}

// DefaultLimits is applied by NewRunner when the caller passes a zero-value
// Limits, so evidence is bounded even when a scenario author does not
// configure a limit explicitly.
var DefaultLimits = Limits{MaxRows: 20, MaxFields: 20}

// RedactedValue replaces a redacted field's value in evidence and terminal
// display. The original value is never copied into the preview.
const RedactedValue = "«redacted»"

// Assertion is one data-state check: which holder and query to run, how to
// canonicalise and redact its rows, and the Expect function that judges the
// canonicalised result. Name is a stable identity copied into Evidence so
// run output can be correlated back to the message, checkpoint or branch
// that attached it (e.g. "add-milk" or "few-items-added").
type Assertion struct {
	// Name is this assertion's stable identity, copied into Evidence.Name.
	Name string
	// Holder names the registered database holder to query. It may be
	// omitted only when the Runner was built with exactly one handle.
	Holder string
	// Query is the canonical DTQL artifact to execute.
	Query Query
	// Normalization controls comparison-field exclusion and row ordering.
	Normalization Normalization
	// Redact lists field names whose values are replaced by RedactedValue
	// in the evidence preview. Redaction never affects Expect, only what is
	// shown or persisted afterwards.
	Redact []string
	// Expect judges the canonicalised rows. A nil Expect always passes: it
	// lets a scenario show a queried record as evidence without also
	// asserting its shape.
	Expect Expectation
}

// Expectation judges normalised rows (after Normalization.ExcludedFields has
// removed the declared comparison-irrelevant fields and rows have been
// deterministically ordered) and returns a descriptive error on failure.
type Expectation func(rows []Row) error

// Evidence is the canonical, JSON-serialisable record of one executed
// data-state assertion: the exact DTQL query and parameters, the holder it
// ran against, its pass/fail outcome, and a bounded, redacted, normalised
// preview of the rows it returned. Every exported field carries an explicit
// lower-camel-case `json` tag — Evidence reaches a run bundle (via
// bundle.AIGoalSection.Evidence) and the whole run-bundle wire is
// uniformly camelCase, so this package's stable shape is the tagged one,
// not Go's default (exported-name) encoding. branching.Evidence and
// goal.CampaignSnapshot are untagged still, but neither reaches the bundle
// wire this package does.
type Evidence struct {
	// Name is the triggering Assertion.Name, correlating this evidence to
	// the message, checkpoint or branch that attached it.
	Name string `json:"name"`
	// AttachmentPoint is where in the scenario this assertion ran.
	AttachmentPoint AttachmentPoint `json:"attachmentPoint"`
	// Holder is the resolved holder name the query ran against (even an
	// unresolved request records the name that was asked for).
	Holder string `json:"holder"`
	// Query is the concrete DTQL text executed.
	Query string `json:"query"`
	// Params is a detached copy of the query's named parameters.
	Params map[string]any `json:"params"`
	// Outcome is OutcomePassed or OutcomeFailed.
	Outcome Outcome `json:"outcome"`
	// FailureMessage is set when Outcome is OutcomeFailed: holder
	// resolution, query execution or Expect's failure message.
	FailureMessage string `json:"failureMessage"`
	// TotalRows is how many rows the query returned before any preview
	// bound was applied.
	TotalRows int `json:"totalRows"`
	// ReturnedRows is how many rows are present in Preview.
	ReturnedRows int `json:"returnedRows"`
	// Truncated is true when Preview omits rows (TotalRows > ReturnedRows)
	// or drops fields from at least one previewed row.
	Truncated bool `json:"truncated"`
	// Preview is the bounded, redacted, normalised recordset: at most
	// Limits.MaxRows rows, each with at most Limits.MaxFields fields.
	// Redacted fields are present with their value replaced by
	// RedactedValue rather than omitted, so evidence still declares which
	// fields exist.
	Preview []Row `json:"preview"`
	// RedactedFields lists the field names configured for redaction (the
	// declared policy), regardless of whether any previewed row contained
	// them.
	RedactedFields []string `json:"redactedFields"`
	// ExcludedFields lists the field names Normalization removed from the
	// comparison basis. They remain visible in Preview: exclusion only
	// means "not part of the assertion", never "hidden from evidence".
	ExcludedFields []string `json:"excludedFields"`
}

var (
	// ErrNilExecutor is returned by NewRunner when executor is nil.
	ErrNilExecutor = errors.New("datastate: executor is nil")
	// ErrEmptyQuery is returned when an assertion's DTQL text is empty.
	ErrEmptyQuery = errors.New("datastate: query DTQL text is empty")
	// ErrNoHolders is returned when a holder name is omitted and no
	// handles are registered.
	ErrNoHolders = errors.New("datastate: no database holders are registered")
	// ErrUnknownHolder is returned when a named holder is not registered.
	ErrUnknownHolder = errors.New("datastate: unknown holder")
	// ErrAmbiguousHolder is returned when a holder name is omitted while
	// more than one handle is registered.
	ErrAmbiguousHolder = errors.New("datastate: holder name is required when more than one holder is registered")
	// ErrAssertionFailed wraps an Expectation's failure.
	ErrAssertionFailed = errors.New("datastate: assertion failed")
)

// resolve looks up the handle named by name, or the sole registered handle
// when name is empty. It never depends on map iteration order for its
// error path, so failures are deterministic across repeated calls.
func (h Handles) resolve(name string) (handle any, resolvedName string, err error) {
	name = strings.TrimSpace(name)
	if name != "" {
		handle, ok := h[name]
		if !ok {
			return nil, "", fmt.Errorf("%w: %s", ErrUnknownHolder, name)
		}
		return handle, name, nil
	}
	switch len(h) {
	case 0:
		return nil, "", ErrNoHolders
	case 1:
		for k, v := range h {
			return v, k, nil
		}
	}
	return nil, "", ErrAmbiguousHolder
}

// Runner executes Assertions against named Handles through an Executor and
// produces bounded, redacted Evidence. Construct a fresh Runner for each
// environment (root or branch) with that environment's own Handles: a
// Runner never outlives the environment it was built for, which is what
// makes "the handle bound to the current environment" hold inside a branch.
type Runner struct {
	executor Executor
	handles  Handles
	limits   Limits
}

// NewRunner creates a Runner. limits is coerced to DefaultLimits when it is
// the zero value, so evidence is always bounded.
func NewRunner(executor Executor, handles Handles, limits Limits) (*Runner, error) {
	if executor == nil {
		return nil, ErrNilExecutor
	}
	if limits == (Limits{}) {
		limits = DefaultLimits
	}
	return &Runner{executor: executor, handles: handles, limits: limits}, nil
}

// Run executes one assertion at the given attachment point and returns its
// Evidence. A non-nil error is returned exactly when Evidence.Outcome is
// OutcomeFailed — holder resolution, query execution and Expect failures are
// all reported the same way — so callers can treat "err != nil" and "did
// not pass" as the same question.
func (r *Runner) Run(ctx context.Context, point AttachmentPoint, assertion Assertion) (Evidence, error) {
	evidence := Evidence{
		Name:            assertion.Name,
		AttachmentPoint: point,
		Holder:          strings.TrimSpace(assertion.Holder),
		Query:           assertion.Query.DTQL,
		Params:          cloneParams(assertion.Query.Params),
		RedactedFields:  append([]string(nil), assertion.Redact...),
		ExcludedFields:  append([]string(nil), assertion.Normalization.ExcludedFields...),
	}

	handle, holderName, err := r.handles.resolve(assertion.Holder)
	if err != nil {
		evidence.Outcome = OutcomeFailed
		evidence.FailureMessage = err.Error()
		return evidence, err
	}
	evidence.Holder = holderName

	if strings.TrimSpace(assertion.Query.DTQL) == "" {
		evidence.Outcome = OutcomeFailed
		evidence.FailureMessage = ErrEmptyQuery.Error()
		return evidence, ErrEmptyQuery
	}

	rows, err := r.executor.Execute(ctx, handle, assertion.Query)
	if err != nil {
		wrapped := fmt.Errorf("datastate: query execution failed: %w", err)
		evidence.Outcome = OutcomeFailed
		evidence.FailureMessage = wrapped.Error()
		return evidence, wrapped
	}

	sorted := sortRows(rows, assertion.Normalization.SortKeys)
	comparison := excludeFields(sorted, assertion.Normalization.ExcludedFields)

	evidence.TotalRows = len(sorted)
	previewRows, truncated := boundPreview(sorted, r.limits)
	evidence.Preview = redactRows(previewRows, assertion.Redact)
	evidence.ReturnedRows = len(previewRows)
	evidence.Truncated = truncated

	if assertion.Expect != nil {
		if expectErr := assertion.Expect(comparison); expectErr != nil {
			wrapped := fmt.Errorf("%w: %s: %v", ErrAssertionFailed, assertion.Name, expectErr)
			evidence.Outcome = OutcomeFailed
			evidence.FailureMessage = wrapped.Error()
			return evidence, wrapped
		}
	}

	evidence.Outcome = OutcomePassed
	return evidence, nil
}

// RunAll runs every assertion in order at the given attachment point and
// stops at the first failure, mirroring the coordinator's "no partial
// publication" rule: it returns the Evidence collected so far — including
// the failing assertion's Evidence — together with that assertion's error.
func (r *Runner) RunAll(ctx context.Context, point AttachmentPoint, assertions ...Assertion) ([]Evidence, error) {
	results := make([]Evidence, 0, len(assertions))
	for _, assertion := range assertions {
		evidence, err := r.Run(ctx, point, assertion)
		results = append(results, evidence)
		if err != nil {
			return results, err
		}
	}
	return results, nil
}

// Gate runs every assertion and calls commit only after all of them pass.
// commit is never invoked once any assertion has failed, and its own error
// (if any) is returned alongside the already-collected evidence.
//
// This is how "a checkpoint publication can require named assertions to
// pass first" is implemented without datastate depending on branching's
// types: pass AttachmentCheckpoint and a commit that performs the actual
// publish step (e.g. registry.Capture, or ExecutionContext.Checkpoint). The
// same method gates a branch-completion check by passing
// AttachmentBranchCompletion and either a continuation or a nil commit when
// there is nothing further to gate.
func (r *Runner) Gate(ctx context.Context, point AttachmentPoint, assertions []Assertion, commit func() error) ([]Evidence, error) {
	results, err := r.RunAll(ctx, point, assertions...)
	if err != nil {
		return results, err
	}
	if commit == nil {
		return results, nil
	}
	if err := commit(); err != nil {
		return results, err
	}
	return results, nil
}

func cloneParams(params map[string]any) map[string]any {
	if params == nil {
		return nil
	}
	cloned := make(map[string]any, len(params))
	for k, v := range params {
		cloned[k] = v
	}
	return cloned
}

func cloneRow(row Row) Row {
	cloned := make(Row, len(row))
	for k, v := range row {
		cloned[k] = v
	}
	return cloned
}

func cloneRows(rows []Row) []Row {
	cloned := make([]Row, len(rows))
	for i, row := range rows {
		cloned[i] = cloneRow(row)
	}
	return cloned
}

// sortRows returns a deterministically ordered copy of rows. It never
// mutates rows or the Row maps within it.
func sortRows(rows []Row, sortKeys []string) []Row {
	sorted := cloneRows(rows)
	sort.SliceStable(sorted, func(i, j int) bool {
		return rowSortKey(sorted[i], sortKeys) < rowSortKey(sorted[j], sortKeys)
	})
	return sorted
}

// rowSortKey returns a stable string key for row: the joined string form of
// sortKeys' values when declared, otherwise row's canonical JSON encoding
// (encoding/json sorts object keys, so this is deterministic regardless of
// Go map iteration order).
func rowSortKey(row Row, sortKeys []string) string {
	if len(sortKeys) > 0 {
		parts := make([]string, len(sortKeys))
		for i, k := range sortKeys {
			parts[i] = fmt.Sprintf("%v", row[k])
		}
		return strings.Join(parts, "\x1f")
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		return fmt.Sprintf("%v", row)
	}
	return string(encoded)
}

// excludeFields returns rows with each field in fields removed, without
// mutating rows or the Row maps within it.
func excludeFields(rows []Row, fields []string) []Row {
	if len(fields) == 0 {
		return cloneRows(rows)
	}
	skip := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		skip[f] = struct{}{}
	}
	out := make([]Row, len(rows))
	for i, row := range rows {
		filtered := make(Row, len(row))
		for k, v := range row {
			if _, excluded := skip[k]; excluded {
				continue
			}
			filtered[k] = v
		}
		out[i] = filtered
	}
	return out
}

// boundPreview caps rows to at most limits.MaxRows entries and each entry to
// at most limits.MaxFields fields, returning the bounded copy and whether
// anything was truncated.
func boundPreview(rows []Row, limits Limits) ([]Row, bool) {
	truncated := false
	n := len(rows)
	if limits.MaxRows > 0 && n > limits.MaxRows {
		n = limits.MaxRows
		truncated = true
	}
	preview := make([]Row, n)
	for i := 0; i < n; i++ {
		row, rowTruncated := boundRowFields(rows[i], limits.MaxFields)
		preview[i] = row
		if rowTruncated {
			truncated = true
		}
	}
	return preview, truncated
}

// boundRowFields caps row to at most maxFields entries, keeping the
// alphabetically first field names so truncation is deterministic.
func boundRowFields(row Row, maxFields int) (Row, bool) {
	if maxFields <= 0 || len(row) <= maxFields {
		return cloneRow(row), false
	}
	keys := make([]string, 0, len(row))
	for k := range row {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	bounded := make(Row, maxFields)
	for _, k := range keys[:maxFields] {
		bounded[k] = row[k]
	}
	return bounded, true
}

// redactRows returns rows with each field named in fields replaced by
// RedactedValue wherever it is present, without mutating rows or the Row
// maps within it.
func redactRows(rows []Row, fields []string) []Row {
	if len(fields) == 0 {
		return cloneRows(rows)
	}
	out := make([]Row, len(rows))
	for i, row := range rows {
		redacted := cloneRow(row)
		for _, f := range fields {
			if _, ok := redacted[f]; ok {
				redacted[f] = RedactedValue
			}
		}
		out[i] = redacted
	}
	return out
}

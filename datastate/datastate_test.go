package datastate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// fakeCall records one Executor.Execute invocation for assertions about
// which handle/query a Runner actually used.
type fakeCall struct {
	handle any
	query  Query
}

// fakeExecutor is the fake DTQL executor used by every contract test in this
// package, per the plan's requirement that Task 2A use a fake executor and
// never require a real DTQL parser or a DataTug process.
type fakeExecutor struct {
	mu      sync.Mutex
	calls   []fakeCall
	handler func(handle any, query Query) ([]Row, error)
}

func (f *fakeExecutor) Execute(_ context.Context, handle any, query Query) ([]Row, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{handle: handle, query: query})
	f.mu.Unlock()
	if f.handler == nil {
		return nil, nil
	}
	return f.handler(handle, query)
}

func (f *fakeExecutor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func staticRows(rows ...Row) func(any, Query) ([]Row, error) {
	return func(any, Query) ([]Row, error) { return rows, nil }
}

func TestAttachmentAfterMessageRecordsPassingEvidence(t *testing.T) {
	exec := &fakeExecutor{handler: staticRows(Row{"title": "milk", "active": true})}
	runner, err := NewRunner(exec, Handles{"listus": "listus-handle"}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	assertion := Assertion{
		Name:   "add-milk",
		Holder: "listus",
		Query:  Query{DTQL: "select from lists where key = buy!groceries", Params: map[string]any{"key": "buy!groceries"}},
		Expect: RowContains(Row{"title": "milk", "active": true}),
	}
	evidence, err := runner.Run(context.Background(), AttachmentAfterMessage, assertion)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if evidence.Outcome != OutcomePassed {
		t.Fatalf("Outcome = %q", evidence.Outcome)
	}
	if evidence.AttachmentPoint != AttachmentAfterMessage {
		t.Fatalf("AttachmentPoint = %q", evidence.AttachmentPoint)
	}
	if evidence.Holder != "listus" {
		t.Fatalf("Holder = %q", evidence.Holder)
	}
	if evidence.Name != "add-milk" {
		t.Fatalf("Name = %q", evidence.Name)
	}
	if evidence.TotalRows != 1 || evidence.ReturnedRows != 1 {
		t.Fatalf("row counts = %d/%d", evidence.TotalRows, evidence.ReturnedRows)
	}
}

func TestNilExpectationAlwaysPasses(t *testing.T) {
	exec := &fakeExecutor{handler: staticRows(Row{"count": 4})}
	runner, _ := NewRunner(exec, Handles{"listus": "h"}, Limits{})
	evidence, err := runner.Run(context.Background(), AttachmentCheckpoint, Assertion{
		Name:   "show-record",
		Holder: "listus",
		Query:  Query{DTQL: "select buy!groceries"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if evidence.Outcome != OutcomePassed {
		t.Fatalf("Outcome = %q", evidence.Outcome)
	}
}

func TestGateBlocksCommitOnFailedAssertion(t *testing.T) {
	exec := &fakeExecutor{handler: staticRows(Row{"title": "milk", "active": false})}
	runner, _ := NewRunner(exec, Handles{"listus": "h"}, Limits{})
	committed := false
	assertions := []Assertion{{
		Name:   "few-items-added",
		Holder: "listus",
		Query:  Query{DTQL: "select buy!groceries"},
		Expect: RowContains(Row{"title": "milk", "active": true}),
	}}
	evidence, err := runner.Gate(context.Background(), AttachmentCheckpoint, assertions, func() error {
		committed = true
		return nil
	})
	if err == nil {
		t.Fatal("Gate() error = nil, want failure")
	}
	if committed {
		t.Fatal("commit was called after a failed assertion")
	}
	if len(evidence) != 1 || evidence[0].Outcome != OutcomeFailed {
		t.Fatalf("evidence = %+v", evidence)
	}
	if !errors.Is(err, ErrAssertionFailed) {
		t.Fatalf("error = %v, want ErrAssertionFailed", err)
	}
}

func TestGateCallsCommitOnlyAfterAllAssertionsPass(t *testing.T) {
	exec := &fakeExecutor{handler: staticRows(Row{"title": "milk", "active": true})}
	runner, _ := NewRunner(exec, Handles{"listus": "h"}, Limits{})
	var order []string
	assertions := []Assertion{
		{Name: "one", Holder: "listus", Query: Query{DTQL: "q1"}, Expect: NonEmpty()},
		{Name: "two", Holder: "listus", Query: Query{DTQL: "q2"}, Expect: NonEmpty()},
	}
	evidence, err := runner.Gate(context.Background(), AttachmentCheckpoint, assertions, func() error {
		order = append(order, "commit")
		return nil
	})
	if err != nil {
		t.Fatalf("Gate() error = %v", err)
	}
	if len(evidence) != 2 || evidence[0].Outcome != OutcomePassed || evidence[1].Outcome != OutcomePassed {
		t.Fatalf("evidence = %+v", evidence)
	}
	if len(order) != 1 || order[0] != "commit" {
		t.Fatalf("commit order = %v, want exactly one commit after both assertions", order)
	}
}

func TestQueryUsesTheResolvedBranchHandle(t *testing.T) {
	const sourceHandle = "source-db"
	const branchHandle = "branch-db"
	exec := &fakeExecutor{handler: staticRows()}
	// Runner bound only to the branch's own replacement handle, never the
	// source or a sibling's — see Handles' doc comment.
	runner, _ := NewRunner(exec, Handles{"listus": branchHandle}, Limits{})
	_, err := runner.Run(context.Background(), AttachmentBranchCompletion, Assertion{
		Holder: "listus",
		Query:  Query{DTQL: "select buy!groceries"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("call count = %d", len(exec.calls))
	}
	got, ok := exec.calls[0].handle.(string)
	if !ok || got != branchHandle {
		t.Fatalf("executed against handle %v, want %v", exec.calls[0].handle, branchHandle)
	}
	if got == sourceHandle {
		t.Fatal("query ran against the source handle")
	}
}

func TestUnresolvedHolderNameIsADeterministicError(t *testing.T) {
	exec := &fakeExecutor{}
	runner, _ := NewRunner(exec, Handles{"listus": "h1", "audit": "h2"}, Limits{})
	assertion := Assertion{Holder: "unknown", Query: Query{DTQL: "select 1"}}
	first, err1 := runner.Run(context.Background(), AttachmentAfterMessage, assertion)
	second, err2 := runner.Run(context.Background(), AttachmentAfterMessage, assertion)
	if !errors.Is(err1, ErrUnknownHolder) || !errors.Is(err2, ErrUnknownHolder) {
		t.Fatalf("errors = %v, %v", err1, err2)
	}
	if err1.Error() != err2.Error() {
		t.Fatalf("non-deterministic error text: %q vs %q", err1.Error(), err2.Error())
	}
	if first.Outcome != OutcomeFailed || second.Outcome != OutcomeFailed {
		t.Fatalf("outcomes = %q, %q", first.Outcome, second.Outcome)
	}
	if exec.callCount() != 0 {
		t.Fatal("query executed despite an unresolved holder")
	}
}

func TestOmittedHolderResolvesWhenExactlyOneRegistered(t *testing.T) {
	exec := &fakeExecutor{handler: staticRows(Row{"ok": true})}
	runner, _ := NewRunner(exec, Handles{"listus": "h1"}, Limits{})
	evidence, err := runner.Run(context.Background(), AttachmentAfterMessage, Assertion{Query: Query{DTQL: "select 1"}})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Holder != "listus" {
		t.Fatalf("Holder = %q", evidence.Holder)
	}
}

func TestOmittedHolderFailsWhenHoldersAreAmbiguous(t *testing.T) {
	exec := &fakeExecutor{}
	runner, _ := NewRunner(exec, Handles{"listus": "h1", "audit": "h2"}, Limits{})
	_, err := runner.Run(context.Background(), AttachmentAfterMessage, Assertion{Query: Query{DTQL: "select 1"}})
	if !errors.Is(err, ErrAmbiguousHolder) {
		t.Fatalf("error = %v", err)
	}
	if exec.callCount() != 0 {
		t.Fatal("query executed despite an ambiguous holder")
	}
}

func TestOmittedHolderFailsWhenNoHoldersRegistered(t *testing.T) {
	exec := &fakeExecutor{}
	runner, _ := NewRunner(exec, Handles{}, Limits{})
	_, err := runner.Run(context.Background(), AttachmentAfterMessage, Assertion{Query: Query{DTQL: "select 1"}})
	if !errors.Is(err, ErrNoHolders) {
		t.Fatalf("error = %v", err)
	}
}

func TestPreviewIsBoundedByRowAndFieldLimits(t *testing.T) {
	rows := make([]Row, 5)
	for i := range rows {
		rows[i] = Row{
			"id": i, "title": fmt.Sprintf("item-%d", i), "active": true, "count": i, "extra": "x",
		}
	}
	exec := &fakeExecutor{handler: staticRows(rows...)}
	runner, _ := NewRunner(exec, Handles{"listus": "h"}, Limits{MaxRows: 2, MaxFields: 2})
	evidence, err := runner.Run(context.Background(), AttachmentAfterMessage, Assertion{
		Holder: "listus",
		Query:  Query{DTQL: "select *"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.TotalRows != 5 {
		t.Fatalf("TotalRows = %d, want 5", evidence.TotalRows)
	}
	if evidence.ReturnedRows != 2 || len(evidence.Preview) != 2 {
		t.Fatalf("ReturnedRows/Preview = %d/%d, want 2/2", evidence.ReturnedRows, len(evidence.Preview))
	}
	if !evidence.Truncated {
		t.Fatal("Truncated = false, want true")
	}
	for _, row := range evidence.Preview {
		if len(row) > 2 {
			t.Fatalf("preview row has %d fields, want at most 2: %v", len(row), row)
		}
	}
}

func TestRedactedFieldsNeverAppearInEvidence(t *testing.T) {
	exec := &fakeExecutor{handler: staticRows(Row{"title": "milk", "ownerToken": "super-secret-value"})}
	runner, _ := NewRunner(exec, Handles{"listus": "h"}, Limits{})
	evidence, err := runner.Run(context.Background(), AttachmentAfterMessage, Assertion{
		Holder: "listus",
		Query:  Query{DTQL: "select *"},
		Redact: []string{"ownerToken"},
		// Expect still sees the real value: redaction bounds evidence, not
		// what the assertion is allowed to check.
		Expect: RowContains(Row{"ownerToken": "super-secret-value"}),
	})
	if err != nil {
		t.Fatalf("Run() error = %v (redaction must not affect Expect)", err)
	}
	if len(evidence.Preview) != 1 || evidence.Preview[0]["ownerToken"] != RedactedValue {
		t.Fatalf("preview ownerToken = %v, want %q", evidence.Preview[0]["ownerToken"], RedactedValue)
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "super-secret-value") {
		t.Fatalf("redacted value leaked into evidence JSON: %s", encoded)
	}
}

func TestRowNormalisationIsOrderDeterministic(t *testing.T) {
	rowA := Row{"title": "apples", "active": true}
	rowB := Row{"title": "bread", "active": true}
	rowC := Row{"title": "milk", "active": true}

	execOne := &fakeExecutor{handler: staticRows(rowC, rowA, rowB)}
	execTwo := &fakeExecutor{handler: staticRows(rowB, rowC, rowA)}

	runnerOne, _ := NewRunner(execOne, Handles{"listus": "h"}, Limits{})
	runnerTwo, _ := NewRunner(execTwo, Handles{"listus": "h"}, Limits{})

	assertion := Assertion{Holder: "listus", Query: Query{DTQL: "select *"}}
	evOne, err := runnerOne.Run(context.Background(), AttachmentAfterMessage, assertion)
	if err != nil {
		t.Fatal(err)
	}
	evTwo, err := runnerTwo.Run(context.Background(), AttachmentAfterMessage, assertion)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(evOne.Preview, evTwo.Preview) {
		t.Fatalf("preview order differs by input order:\n%v\n%v", evOne.Preview, evTwo.Preview)
	}

	// An explicit sort key also produces a deterministic, predictable order.
	sortedAssertion := assertion
	sortedAssertion.Normalization = Normalization{SortKeys: []string{"title"}}
	evSorted, err := runnerOne.Run(context.Background(), AttachmentAfterMessage, sortedAssertion)
	if err != nil {
		t.Fatal(err)
	}
	wantTitles := []string{"apples", "bread", "milk"}
	for i, row := range evSorted.Preview {
		if row["title"] != wantTitles[i] {
			t.Fatalf("sorted preview[%d].title = %v, want %v", i, row["title"], wantTitles[i])
		}
	}
}

func TestExcludedFieldsAreVisibleButDoNotAffectExpectation(t *testing.T) {
	execOne := &fakeExecutor{handler: staticRows(Row{"id": "aaa111", "title": "milk", "active": true})}
	execTwo := &fakeExecutor{handler: staticRows(Row{"id": "bbb222", "title": "milk", "active": true})}

	want := []Row{{"title": "milk", "active": true}}
	assertion := Assertion{
		Holder:        "listus",
		Query:         Query{DTQL: "select *"},
		Normalization: Normalization{ExcludedFields: []string{"id"}},
		Expect:        ExactRows(want),
	}

	runnerOne, _ := NewRunner(execOne, Handles{"listus": "h"}, Limits{})
	runnerTwo, _ := NewRunner(execTwo, Handles{"listus": "h"}, Limits{})

	evOne, err := runnerOne.Run(context.Background(), AttachmentCheckpoint, assertion)
	if err != nil {
		t.Fatalf("Run() with differing excluded id failed: %v", err)
	}
	evTwo, err := runnerTwo.Run(context.Background(), AttachmentCheckpoint, assertion)
	if err != nil {
		t.Fatalf("Run() with differing excluded id failed: %v", err)
	}
	if evOne.Preview[0]["id"] != "aaa111" || evTwo.Preview[0]["id"] != "bbb222" {
		t.Fatalf("excluded field must still be visible in evidence: %v / %v", evOne.Preview, evTwo.Preview)
	}
	if len(evOne.ExcludedFields) != 1 || evOne.ExcludedFields[0] != "id" {
		t.Fatalf("ExcludedFields = %v", evOne.ExcludedFields)
	}
}

func TestEvidenceRecordsCanonicalQueryAndParams(t *testing.T) {
	exec := &fakeExecutor{handler: staticRows(Row{"count": 4})}
	runner, _ := NewRunner(exec, Handles{"listus": "h"}, Limits{})
	params := map[string]any{"space": "family-1", "key": "buy!groceries"}
	evidence, err := runner.Run(context.Background(), AttachmentCheckpoint, Assertion{
		Name:   "few-items-added",
		Holder: "listus",
		Query:  Query{DTQL: "select from lists where parent = :space and key = :key", Params: params},
	})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Query != "select from lists where parent = :space and key = :key" {
		t.Fatalf("Query = %q", evidence.Query)
	}
	if !reflect.DeepEqual(evidence.Params, params) {
		t.Fatalf("Params = %v, want %v", evidence.Params, params)
	}
	// Evidence.Params must be a detached copy, not an alias.
	params["space"] = "mutated"
	if evidence.Params["space"] == "mutated" {
		t.Fatal("Evidence.Params aliases the caller's map")
	}

	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"name", "attachmentPoint", "holder", "query", "params", "outcome", "totalRows", "returnedRows", "truncated", "preview"} {
		if _, ok := decoded[field]; !ok {
			t.Fatalf("evidence JSON missing field %q: %s", field, encoded)
		}
	}
}

func TestQueryExecutionErrorFailsExplicitly(t *testing.T) {
	wantErr := errors.New("unsupported: parent-scoped collection group")
	exec := &fakeExecutor{handler: func(any, Query) ([]Row, error) { return nil, wantErr }}
	runner, _ := NewRunner(exec, Handles{"listus": "h"}, Limits{})
	evidence, err := runner.Run(context.Background(), AttachmentAfterMessage, Assertion{
		Holder: "listus",
		Query:  Query{DTQL: "select from lists group by parent"},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want wrapping %v", err, wantErr)
	}
	if evidence.Outcome != OutcomeFailed || evidence.TotalRows != 0 || len(evidence.Preview) != 0 {
		t.Fatalf("evidence = %+v", evidence)
	}
}

func TestEmptyQueryTextFailsExplicitly(t *testing.T) {
	exec := &fakeExecutor{}
	runner, _ := NewRunner(exec, Handles{"listus": "h"}, Limits{})
	_, err := runner.Run(context.Background(), AttachmentAfterMessage, Assertion{Holder: "listus"})
	if !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("error = %v", err)
	}
	if exec.callCount() != 0 {
		t.Fatal("executor called despite empty query text")
	}
}

func TestNewRunnerRejectsNilExecutor(t *testing.T) {
	if _, err := NewRunner(nil, Handles{}, Limits{}); !errors.Is(err, ErrNilExecutor) {
		t.Fatalf("error = %v", err)
	}
}

func TestZeroLimitsCoerceToDefaultLimits(t *testing.T) {
	runner, err := NewRunner(&fakeExecutor{}, Handles{}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if runner.limits != DefaultLimits {
		t.Fatalf("limits = %+v, want %+v", runner.limits, DefaultLimits)
	}
}

func TestRowContainsMatchesNestedShape(t *testing.T) {
	record := Row{
		"key":   "buy!groceries",
		"count": 4,
		"items": []any{
			map[string]any{"title": "milk", "active": true},
			map[string]any{"title": "bread", "active": true},
		},
	}
	exec := &fakeExecutor{handler: staticRows(record)}
	runner, _ := NewRunner(exec, Handles{"listus": "h"}, Limits{})

	evidence, err := runner.Run(context.Background(), AttachmentAfterMessage, Assertion{
		Holder: "listus",
		Query:  Query{DTQL: "select buy!groceries"},
		Expect: RowContains(Row{
			"items": []any{
				map[string]any{"title": "milk", "active": true},
				map[string]any{"title": "bread", "active": true},
			},
		}),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if evidence.Outcome != OutcomePassed {
		t.Fatalf("Outcome = %q", evidence.Outcome)
	}

	// A mismatched nested shape must fail explicitly, not pass silently.
	_, err = runner.Run(context.Background(), AttachmentAfterMessage, Assertion{
		Holder: "listus",
		Query:  Query{DTQL: "select buy!groceries"},
		Expect: RowContains(Row{
			"items": []any{map[string]any{"title": "milk", "active": false}},
		}),
	})
	if err == nil {
		t.Fatal("expected a mismatch error, got nil")
	}
}

func TestExactRowsComparesCanonicalRowsetRegardlessOfOrder(t *testing.T) {
	rows := []Row{{"title": "bread"}, {"title": "milk"}}
	want := []Row{{"title": "milk"}, {"title": "bread"}}
	if err := ExactRows(want)(rows); err != nil {
		t.Fatalf("ExactRows() = %v, want nil", err)
	}
	mismatched := []Row{{"title": "milk"}, {"title": "eggs"}}
	if err := ExactRows(want)(mismatched); err == nil {
		t.Fatal("ExactRows() = nil, want a mismatch error")
	}
}

func TestRunAllStopsAtFirstFailure(t *testing.T) {
	exec := &fakeExecutor{handler: staticRows(Row{"active": false})}
	runner, _ := NewRunner(exec, Handles{"listus": "h"}, Limits{})
	assertions := []Assertion{
		{Name: "first", Holder: "listus", Query: Query{DTQL: "q1"}, Expect: RowContains(Row{"active": true})},
		{Name: "second", Holder: "listus", Query: Query{DTQL: "q2"}, Expect: NonEmpty()},
	}
	evidence, err := runner.RunAll(context.Background(), AttachmentBranchCompletion, assertions...)
	if err == nil {
		t.Fatal("RunAll() error = nil, want failure")
	}
	if len(evidence) != 1 {
		t.Fatalf("evidence = %+v, want exactly the failing assertion's evidence", evidence)
	}
	if exec.callCount() != 1 {
		t.Fatalf("call count = %d, want 1 (the second assertion must not run)", exec.callCount())
	}
}

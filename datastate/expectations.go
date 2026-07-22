package datastate

import (
	"fmt"
	"reflect"
)

// ExactRowCount asserts rows has exactly n entries.
func ExactRowCount(n int) Expectation {
	return func(rows []Row) error {
		if len(rows) != n {
			return fmt.Errorf("want exactly %d row(s), got %d", n, len(rows))
		}
		return nil
	}
}

// MinRowCount asserts rows has at least n entries.
func MinRowCount(n int) Expectation {
	return func(rows []Row) error {
		if len(rows) < n {
			return fmt.Errorf("want at least %d row(s), got %d", n, len(rows))
		}
		return nil
	}
}

// MaxRowCount asserts rows has at most n entries.
func MaxRowCount(n int) Expectation {
	return func(rows []Row) error {
		if len(rows) > n {
			return fmt.Errorf("want at most %d row(s), got %d", n, len(rows))
		}
		return nil
	}
}

// NonEmpty asserts rows has at least one entry.
func NonEmpty() Expectation { return MinRowCount(1) }

// Empty asserts rows has no entries.
func Empty() Expectation { return ExactRowCount(0) }

// RowContains asserts at least one row deep-contains every field in
// partial: nested maps are matched the same way and nested slices must
// match element-for-element. This is the "row containing a partial
// field/value shape, including nested maps/lists" assertion the
// data-state-assertions feature requires for the Listus proof (an active
// item embedded in a list record's `items` field).
func RowContains(partial Row) Expectation {
	return func(rows []Row) error {
		for _, row := range rows {
			if partialMatch(map[string]any(partial), map[string]any(row)) {
				return nil
			}
		}
		return fmt.Errorf("no row contains expected shape %v", map[string]any(partial))
	}
}

// AllRows asserts every row satisfies check.
func AllRows(check func(Row) error) Expectation {
	return func(rows []Row) error {
		for i, row := range rows {
			if err := check(row); err != nil {
				return fmt.Errorf("row %d: %w", i, err)
			}
		}
		return nil
	}
}

// AnyRow asserts at least one row satisfies check.
func AnyRow(check func(Row) error) Expectation {
	return func(rows []Row) error {
		var last error
		for _, row := range rows {
			if err := check(row); err == nil {
				return nil
			} else {
				last = err
			}
		}
		if last == nil {
			last = fmt.Errorf("no rows returned")
		}
		return fmt.Errorf("no row satisfied the check: %w", last)
	}
}

// All combines expectations: every one must pass, in order. A nil entry is
// skipped.
func All(expectations ...Expectation) Expectation {
	return func(rows []Row) error {
		for _, expect := range expectations {
			if expect == nil {
				continue
			}
			if err := expect(rows); err != nil {
				return err
			}
		}
		return nil
	}
}

// ExactRows asserts rows exactly equals want after both are put through the
// same deterministic canonical ordering ExactRows uses internally — the
// caller does not need want to already be in the order the runner will
// return rows in.
func ExactRows(want []Row) Expectation {
	return func(rows []Row) error {
		gotSorted := sortRows(rows, nil)
		wantSorted := sortRows(want, nil)
		if !reflect.DeepEqual(gotSorted, wantSorted) {
			return fmt.Errorf("rowset mismatch: want %v, got %v", wantSorted, gotSorted)
		}
		return nil
	}
}

func partialMatch(expected, actual map[string]any) bool {
	for k, v := range expected {
		av, ok := actual[k]
		if !ok || !partialMatchValue(v, av) {
			return false
		}
	}
	return true
}

func partialMatchValue(expected, actual any) bool {
	switch exp := expected.(type) {
	case Row:
		return partialMatchValue(map[string]any(exp), actual)
	case map[string]any:
		act, ok := actual.(map[string]any)
		if !ok {
			if actRow, isRow := actual.(Row); isRow {
				act = map[string]any(actRow)
			} else {
				return false
			}
		}
		return partialMatch(exp, act)
	case []any:
		act, ok := actual.([]any)
		if !ok || len(act) != len(exp) {
			return false
		}
		for i := range exp {
			if !partialMatchValue(exp[i], act[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(expected, actual)
	}
}

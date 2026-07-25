package scenario

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"chatwright.dev/runtime/platform"
)

// UnmetPrefix is the fixed runtime prefix an unmet VerifyResult's Detail
// always starts with — the README's "the verdict maps to VerifyResult ...
// the fixed runtime prefix journal evidence incomplete: " rule. It is
// pinned as an exported constant, not reconstructed at each call site, so
// this package and its tests can never drift from arena's own
// verifyGreetbotJournal, which produces the identical string for the
// identical reason.
const UnmetPrefix = "journal evidence incomplete: "

// VerifyResult is one Evaluate call's deterministic verdict — the wire
// equivalent of arena.VerifyResult, kept as this package's own type so
// scenario never imports arena (a lower-level package importing a
// higher-level one).
type VerifyResult struct {
	Verified bool
	Detail   string
}

// VerifySpec is a Document's Verify block, compiled once (regex patterns
// parsed, condition values decoded) so Evaluate can run against a journal
// with no further parsing.
type VerifySpec struct {
	chatDocID    string
	metDetail    string
	expectations []compiledExpectation
}

// ChatDocID is the doc-local chat id (Document.Chats[*].ID) this VerifySpec
// evaluates against — the caller resolves it to a platform chat id via the
// same Document.Chats mapping Build uses, and reads that chat's journal.
func (s *VerifySpec) ChatDocID() string { return s.chatDocID }

type compiledExpectation struct {
	id          string
	unmetDetail string
	all         []compiledCondition
}

type compiledCondition struct {
	field  string
	op     string
	values []string // exact/contains: any-of
	bool_  bool     // exact on the "edited" field
	regex  *regexp.Regexp
	negate bool
}

// CompileVerify compiles doc.Verify into a VerifySpec ready to Evaluate, or
// (nil, nil) when doc declares no Verify block at all — see the README's
// "A document with no verify block is not reported as verified" rule; the
// caller (Build) is what turns a nil VerifySpec into the judged-not-
// verified outcome, this function only reports absence.
//
// CompileVerify assumes doc has already passed validate (see Parse):
// Condition.Field/Op vocabulary, the regex subset restriction and
// Value's shape are not re-checked here — a malformed Verify block reaching
// this function unvalidated is a caller bug, reported as a plain Go error
// rather than a Report Issue.
func CompileVerify(doc *Document) (*VerifySpec, error) {
	if doc.Verify == nil {
		return nil, nil
	}
	v := doc.Verify
	spec := &VerifySpec{chatDocID: v.Chat, metDetail: v.MetDetail}
	for _, exp := range v.Journal {
		compiledAll := make([]compiledCondition, 0, len(exp.All))
		for _, c := range exp.All {
			cc, err := compileCondition(c)
			if err != nil {
				return nil, fmt.Errorf("scenario: verify.journal[%s]: %w", exp.ID, err)
			}
			compiledAll = append(compiledAll, cc)
		}
		spec.expectations = append(spec.expectations, compiledExpectation{
			id: exp.ID, unmetDetail: exp.UnmetDetail, all: compiledAll,
		})
	}
	return spec, nil
}

func compileCondition(c Condition) (compiledCondition, error) {
	cc := compiledCondition{field: c.Field, op: c.Op, negate: c.Negate}
	if c.Field == FieldEdited {
		if err := json.Unmarshal(c.Value, &cc.bool_); err != nil {
			return cc, fmt.Errorf("condition on %q: value must be a boolean: %w", c.Field, err)
		}
		return cc, nil
	}
	if c.Op == OpRegex {
		var pattern string
		if err := json.Unmarshal(c.Value, &pattern); err != nil {
			return cc, fmt.Errorf("regex condition: value must be a string: %w", err)
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return cc, fmt.Errorf("regex condition: %w", err)
		}
		cc.regex = re
		return cc, nil
	}
	values, err := decodeStringOrStringList(c.Value)
	if err != nil {
		return cc, fmt.Errorf("condition on %q: %w", c.Field, err)
	}
	cc.values = values
	return cc, nil
}

func decodeStringOrStringList(raw json.RawMessage) ([]string, error) {
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return []string{single}, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	return nil, fmt.Errorf("value must be a string or an array of strings")
}

// Evaluate re-derives a Verify block's verdict from entries — one chat's
// complete platform.JournalEntry history — independent of any actor's own
// task-done claim (principle 3, "evidence over claims"). Expectations are
// matched in declared order: expectation N matches the earliest entry
// strictly after the entry expectation N-1 matched (or, if N-1 never
// matched, after whatever the last successfully matched expectation did) —
// see the README's "Expectations are ordered" rule. All expectations
// matched yields Verified:true with Detail set to the spec's own
// MetDetail; otherwise Verified:false with Detail set to UnmetPrefix
// followed by every unmatched expectation's UnmetDetail, in declared
// order, joined with "; ".
func (s *VerifySpec) Evaluate(entries []platform.JournalEntry) VerifyResult {
	cursor := -1
	var unmet []string
	for _, exp := range s.expectations {
		idx := findMatch(entries, cursor+1, exp)
		if idx < 0 {
			unmet = append(unmet, exp.unmetDetail)
			continue
		}
		cursor = idx
	}
	if len(unmet) == 0 {
		return VerifyResult{Verified: true, Detail: s.metDetail}
	}
	return VerifyResult{Verified: false, Detail: UnmetPrefix + strings.Join(unmet, "; ")}
}

func findMatch(entries []platform.JournalEntry, from int, exp compiledExpectation) int {
	for i := from; i < len(entries); i++ {
		if matchAll(entries[i], exp.all) {
			return i
		}
	}
	return -1
}

func matchAll(e platform.JournalEntry, conditions []compiledCondition) bool {
	for _, c := range conditions {
		if !matchCondition(e, c) {
			return false
		}
	}
	return true
}

func matchCondition(e platform.JournalEntry, c compiledCondition) bool {
	var result bool
	if c.field == FieldEdited {
		result = (e.Version > 0) == c.bool_
	} else {
		actual := fieldValue(e, c.field)
		switch c.op {
		case OpExact:
			result = matchesAny(c.values, actual, false)
		case OpContains:
			result = matchesAny(c.values, actual, true)
		case OpRegex:
			result = c.regex.MatchString(actual)
		}
	}
	if c.negate {
		result = !result
	}
	return result
}

func fieldValue(e platform.JournalEntry, field string) string {
	switch field {
	case FieldKind:
		return string(e.Kind)
	case FieldDirection:
		return string(e.Direction)
	case FieldText:
		return e.Text
	default:
		return ""
	}
}

func matchesAny(candidates []string, actual string, substr bool) bool {
	for _, c := range candidates {
		if substr {
			if strings.Contains(actual, c) {
				return true
			}
		} else if actual == c {
			return true
		}
	}
	return false
}

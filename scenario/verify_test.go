package scenario

import (
	"encoding/json"
	"testing"
	"time"

	"chatwright.dev/runtime/platform"
)

func msg(direction platform.Direction, text string, version int) platform.JournalEntry {
	return platform.JournalEntry{Kind: platform.JournalEntryMessage, Direction: direction, Text: text, Version: version, At: time.Time{}}
}

func TestVerifySpec_OrderedExpectationsAllMet(t *testing.T) {
	doc := &Document{Verify: &Verify{
		Chat:      "main",
		MetDetail: "all good",
		Journal: []JournalExpectation{
			{ID: "a", UnmetDetail: "no a", All: []Condition{
				{Field: FieldDirection, Op: OpExact, Value: rawString("user")},
				{Field: FieldText, Op: OpExact, Value: rawString("/start")},
			}},
			{ID: "b", UnmetDetail: "no b", All: []Condition{
				{Field: FieldDirection, Op: OpExact, Value: rawString("bot")},
				{Field: FieldEdited, Op: OpExact, Value: rawBool(true)},
			}},
		},
	}}
	spec, err := CompileVerify(doc)
	if err != nil {
		t.Fatalf("CompileVerify() error = %v", err)
	}

	entries := []platform.JournalEntry{
		msg(platform.DirectionUser, "/start", 0),
		msg(platform.DirectionBot, "picker", 0),
		msg(platform.DirectionBot, "Howdy stranger", 1), // edited
	}
	got := spec.Evaluate(entries)
	if !got.Verified {
		t.Fatalf("Evaluate() = %+v, want Verified=true", got)
	}
	if got.Detail != "all good" {
		t.Errorf("Detail = %q, want the spec's own MetDetail", got.Detail)
	}
}

func TestVerifySpec_UnmetExpectationsJoinInDeclaredOrderWithPinnedPrefix(t *testing.T) {
	doc := &Document{Verify: &Verify{
		Chat: "main", MetDetail: "met",
		Journal: []JournalExpectation{
			{ID: "a", UnmetDetail: "never sent /start", All: []Condition{
				{Field: FieldText, Op: OpExact, Value: rawString("/start")},
			}},
			{ID: "b", UnmetDetail: "never greeted", All: []Condition{
				{Field: FieldText, Op: OpExact, Value: rawString("Howdy stranger")},
			}},
		},
	}}
	spec, err := CompileVerify(doc)
	if err != nil {
		t.Fatalf("CompileVerify() error = %v", err)
	}

	got := spec.Evaluate(nil) // empty journal: both expectations unmet
	if got.Verified {
		t.Fatal("Evaluate() Verified = true, want false for an empty journal")
	}
	want := UnmetPrefix + "never sent /start; never greeted"
	if got.Detail != want {
		t.Errorf("Detail = %q, want %q", got.Detail, want)
	}
}

// TestVerifySpec_OrderingIsStrict proves expectation N must match strictly
// after the entry expectation N-1 matched — an entry that would satisfy
// expectation 2 but appears BEFORE the entry satisfying expectation 1 must
// not let expectation 2 match against it.
func TestVerifySpec_OrderingIsStrict(t *testing.T) {
	doc := &Document{Verify: &Verify{
		Chat: "main", MetDetail: "met",
		Journal: []JournalExpectation{
			{ID: "first", UnmetDetail: "no first", All: []Condition{{Field: FieldText, Op: OpExact, Value: rawString("A")}}},
			{ID: "second", UnmetDetail: "no second", All: []Condition{{Field: FieldText, Op: OpExact, Value: rawString("B")}}},
		},
	}}
	spec, err := CompileVerify(doc)
	if err != nil {
		t.Fatalf("CompileVerify() error = %v", err)
	}

	// "B" appears before "A" in the journal — must not verify.
	entries := []platform.JournalEntry{
		msg(platform.DirectionUser, "B", 0),
		msg(platform.DirectionUser, "A", 0),
	}
	got := spec.Evaluate(entries)
	if got.Verified {
		t.Fatal("Evaluate() Verified = true, want false: \"B\" occurs before \"A\" and must not satisfy the second expectation")
	}

	// The correctly ordered journal does verify.
	entries2 := []platform.JournalEntry{
		msg(platform.DirectionUser, "A", 0),
		msg(platform.DirectionUser, "B", 0),
	}
	got2 := spec.Evaluate(entries2)
	if !got2.Verified {
		t.Fatalf("Evaluate() = %+v, want Verified=true for the correctly ordered journal", got2)
	}
}

func TestVerifySpec_NegateAndRegex(t *testing.T) {
	doc := &Document{Verify: &Verify{
		Chat: "main", MetDetail: "met",
		Journal: []JournalExpectation{
			{ID: "ack", UnmetDetail: "no ack", All: []Condition{
				{Field: FieldText, Op: OpRegex, Value: rawString(`\S`)},
				{Field: FieldText, Op: OpExact, Value: rawString("/start"), Negate: true},
			}},
		},
	}}
	spec, err := CompileVerify(doc)
	if err != nil {
		t.Fatalf("CompileVerify() error = %v", err)
	}

	if got := spec.Evaluate([]platform.JournalEntry{msg(platform.DirectionUser, "/start", 0)}); got.Verified {
		t.Fatal("Evaluate() Verified = true for \"/start\" alone, want false (negate excludes it)")
	}
	if got := spec.Evaluate([]platform.JournalEntry{msg(platform.DirectionUser, "Thanks!", 0)}); !got.Verified {
		t.Fatalf("Evaluate() = %+v, want Verified=true for \"Thanks!\"", got)
	}
}

func TestCompileVerify_NilForNoVerifyBlock(t *testing.T) {
	spec, err := CompileVerify(&Document{})
	if err != nil {
		t.Fatalf("CompileVerify() error = %v", err)
	}
	if spec != nil {
		t.Fatal("CompileVerify() returned a non-nil VerifySpec for a document with no Verify block")
	}
}

func rawString(s string) json.RawMessage { b, _ := json.Marshal(s); return b }
func rawBool(b bool) json.RawMessage     { v, _ := json.Marshal(b); return v }

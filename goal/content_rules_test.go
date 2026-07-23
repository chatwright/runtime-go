package goal

import (
	"context"
	"errors"
	"regexp"
	"testing"
)

func TestContentRulesEmpty(t *testing.T) {
	if !(ContentRules{}).Empty() {
		t.Fatal("zero-value ContentRules.Empty() = false, want true")
	}
	if (ContentRules{Vocabulary: []string{"milk"}}).Empty() {
		t.Fatal("ContentRules with Vocabulary.Empty() = true, want false")
	}
	if (ContentRules{DenyPatterns: []*regexp.Regexp{regexp.MustCompile("x")}}).Empty() {
		t.Fatal("ContentRules with DenyPatterns.Empty() = true, want false")
	}
	if (ContentRules{Predicate: func(context.Context, string) (bool, string, error) { return true, "", nil }}).Empty() {
		t.Fatal("ContentRules with Predicate.Empty() = true, want false")
	}
}

// TestContentRulesCheckVocabulary proves a groceries-style vocabulary
// allowlist blocks off-domain text (the "add plasma TV" proof shape) and
// passes on-domain text, exactly the exact/deterministic matching the idea
// requires (no semantic judgement).
func TestContentRulesCheckVocabulary(t *testing.T) {
	rules := ContentRules{Vocabulary: []string{"milk", "eggs", "bread"}}

	ok, reason, err := rules.Check(context.Background(), "add milk to the list")
	if err != nil || !ok {
		t.Fatalf("Check(add milk) = %v, %q, %v, want true, \"\", nil", ok, reason, err)
	}

	ok, reason, err = rules.Check(context.Background(), "add a plasma TV")
	if err != nil {
		t.Fatalf("Check(add plasma TV) error = %v", err)
	}
	if ok {
		t.Fatal("Check(add plasma TV) = true, want false — TV is not in the groceries vocabulary")
	}
	if reason == "" {
		t.Fatal("Check(add plasma TV) reason is empty, want an explanation")
	}
}

func TestContentRulesCheckDenyPatternWinsOverVocabulary(t *testing.T) {
	rules := ContentRules{
		Vocabulary:   []string{"milk"},
		DenyPatterns: []*regexp.Regexp{regexp.MustCompile(`(?i)delete`)},
	}
	ok, reason, err := rules.Check(context.Background(), "delete all milk")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if ok {
		t.Fatal("Check(delete all milk) = true, want false — deny-patterns are checked first")
	}
	if reason == "" {
		t.Fatal("Check() reason is empty, want an explanation naming the denied pattern")
	}
}

func TestContentRulesCheckPredicate(t *testing.T) {
	predErr := errors.New("predicate boom")
	rules := ContentRules{
		Predicate: func(_ context.Context, text string) (bool, string, error) {
			if text == "boom" {
				return false, "", predErr
			}
			return text == "ok", "predicate says no", nil
		},
	}

	if ok, _, err := rules.Check(context.Background(), "ok"); err != nil || !ok {
		t.Fatalf("Check(ok) = %v, %v, want true, nil", ok, err)
	}
	if ok, reason, err := rules.Check(context.Background(), "nope"); err != nil || ok || reason != "predicate says no" {
		t.Fatalf("Check(nope) = %v, %q, %v, want false, \"predicate says no\", nil", ok, reason, err)
	}
	if _, _, err := rules.Check(context.Background(), "boom"); !errors.Is(err, predErr) {
		t.Fatalf("Check(boom) error = %v, want it to wrap the predicate's own error", err)
	}
}

func TestContentRulesCheckEmptyAlwaysPasses(t *testing.T) {
	ok, reason, err := ContentRules{}.Check(context.Background(), "literally anything")
	if err != nil || !ok || reason != "" {
		t.Fatalf("Check() on the zero value = %v, %q, %v, want true, \"\", nil", ok, reason, err)
	}
}

// TestEffectiveContentRulesTaskOverridesGoal proves the idea's own open
// question ("Rule scope: per-task, per-goal, or both with task overriding
// goal?") is resolved as: both are supported, and a non-empty Task rule
// entirely overrides (never merges with) its Goal's.
func TestEffectiveContentRulesTaskOverridesGoal(t *testing.T) {
	goalRules := ContentRules{Vocabulary: []string{"goal-term"}}
	taskRules := ContentRules{Vocabulary: []string{"task-term"}}

	g := Goal{ID: "g", ContentRules: goalRules, Tasks: []Task{
		{ID: "inherits"},
		{ID: "overrides", ContentRules: taskRules},
	}}

	got := EffectiveContentRules(g, g.Tasks[0])
	if len(got.Vocabulary) != 1 || got.Vocabulary[0] != "goal-term" {
		t.Fatalf("EffectiveContentRules(inherits) = %+v, want the goal's own rules", got)
	}

	got = EffectiveContentRules(g, g.Tasks[1])
	if len(got.Vocabulary) != 1 || got.Vocabulary[0] != "task-term" {
		t.Fatalf("EffectiveContentRules(overrides) = %+v, want the task's own rules, not merged with the goal's", got)
	}
}

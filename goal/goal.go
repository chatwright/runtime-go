// Package goal is Chatwright's goal/task/budget contract for goal-driven AI
// testing: the campaign's product-level intent (Goal), its trackable units
// of work (Task) with dependencies and prose success criteria, the limits
// that bound an autonomous run (Budgets), and the guarded state machine that
// tracks progress against them (CampaignState).
//
// This package is a pure contract: no AI, no emulator, no I/O. It has no
// opinion on how a task is attempted — only on what a valid Goal looks like
// and which state transitions a campaign may legally make. The
// observe-plan-act-validate loop that drives an AI actor through a
// CampaignState (package actor) is a later slice. This package does depend
// on observe (for Criteria's Observation parameter — see Task.Criteria):
// observe is itself I/O-free, a pure projection over data the platform
// package already read, so this dependency does not compromise "no I/O"; it
// stays one-directional (observe never imports goal).
//
// Typical use:
//
//	g := goal.Goal{
//		ID:    "listus-shopping-list",
//		Title: "Exercise the shopping-list lifecycle",
//		Tasks: []goal.Task{
//			{ID: "onboarding", SuccessCriteria: "user completes language selection"},
//			{ID: "add-items", DependsOn: []string{"onboarding"}, SuccessCriteria: "several items visible in the list"},
//		},
//		Budgets: goal.Budgets{MaxSteps: 80, MaxDuration: 10 * time.Minute, MaxRepeatedFailures: 3},
//	}
//	campaign, err := goal.NewCampaignState(g, time.Now)
//	// campaign.Activate("onboarding") ... campaign.Complete("onboarding") ...
package goal

import (
	"context"
	"fmt"
	"strings"

	"chatwright.dev/runtime/observe"
)

// Criteria is an optional, machine-checkable predicate for a Task's
// completion: given the current observation, it reports whether the
// task's success condition already holds. This is the loop-side backstop
// spec/ideas/evidence-defined-completion.md describes, alongside (never
// instead of) the prose SuccessCriteria the actor itself reads — see
// Task.Criteria.
//
// A Criteria closure may also express a datastate assertion via the
// existing datastate.Executor seam (spec/ideas/evidence-defined-completion.md's
// "and/or a datastate assertion") by capturing a *datastate.Runner and
// ignoring obs entirely; this package depends on neither datastate nor any
// concrete executor to keep that seam open without importing it here.
//
// Returning (false, nil) means "not yet met" — the ordinary, expected
// outcome on most iterations, never treated as an error. A non-nil error
// means evaluation itself failed (e.g. a query execution error, as
// distinct from an assertion that ran and did not hold) and is surfaced to
// the caller driving the loop, not silently treated as "not met".
type Criteria func(ctx context.Context, obs observe.Observation) (bool, error)

// Task is one trackable unit of work inside a Goal. Success is judged by
// prose SuccessCriteria — the contract never prescribes the bot commands or
// callback data used to satisfy it. DependsOn names other Task IDs in the
// same Goal that must be Completed before this task becomes eligible for
// CampaignState.Activate. Milestones names checkpoints this task's
// completion may reach; the reporting layer, not this package, interprets
// them.
type Task struct {
	ID              string   `json:"id"`
	Title           string   `json:"title"`
	DependsOn       []string `json:"dependsOn"`
	SuccessCriteria string   `json:"successCriteria"`
	Milestones      []string `json:"milestones"`

	// Criteria is this task's optional machine-checkable completion seam —
	// Go-only, never serialised (the wire's Task shape is unchanged: this
	// field carries no `json` tag other than "-"). When set, the loop
	// evaluates it after every executed action and completes the task
	// deterministically the moment it holds — see
	// spec/ideas/evidence-defined-completion.md. Nil means "prose only",
	// the pre-existing behaviour: the actor's own task-done proposal (and
	// budgets, as the ultimate backstop) are all that end the task.
	Criteria Criteria `json:"-"`

	// ContentRules is this task's optional machine-checkable content seam
	// — Go-only, never serialised. When set (non-empty), it overrides the
	// owning Goal's own ContentRules for this task entirely rather than
	// merging with it — see EffectiveContentRules and
	// spec/ideas/proposal-content-constraints.md's own open question on
	// rule scope ("task overriding goal").
	ContentRules ContentRules `json:"-"`
}

// Goal is one campaign's product-level intent: a natural-language outcome
// broken into Tasks, plus the Constraints and Budgets that bound how an
// actor may pursue it. A Goal describes intent, never platform mechanics —
// see the goal-and-task-contract feature's
// goal-does-not-leak-platform-mechanics acceptance criterion.
type Goal struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Tasks       []Task   `json:"tasks"`
	Constraints []string `json:"constraints"`
	Budgets     Budgets  `json:"budgets"`

	// ContentRules is this goal's optional machine-checkable content seam
	// — Go-only, never serialised — applied to every Task that does not
	// declare its own (non-empty) ContentRules. See
	// EffectiveContentRules.
	ContentRules ContentRules `json:"-"`
}

// Validate checks that g is well-formed:
//
//   - every Task has a non-empty, unique ID;
//   - every Task.DependsOn entry resolves to another Task ID in g;
//   - the dependency graph is acyclic;
//   - Budgets are non-negative, and MaxCost, if set, is positive.
//
// NewCampaignState calls Validate during construction, so a CampaignState
// can never exist over an invalid Goal. Callers may also call it directly —
// for example to validate an authored goal before scheduling a campaign.
func (g Goal) Validate() error {
	ids := make(map[string]struct{}, len(g.Tasks))
	for i, task := range g.Tasks {
		if strings.TrimSpace(task.ID) == "" {
			return fmt.Errorf("%w: task at index %d", ErrEmptyTaskID, i)
		}
		if _, exists := ids[task.ID]; exists {
			return fmt.Errorf("%w: %s", ErrDuplicateTaskID, task.ID)
		}
		ids[task.ID] = struct{}{}
	}
	for _, task := range g.Tasks {
		for _, dep := range task.DependsOn {
			if _, ok := ids[dep]; !ok {
				return fmt.Errorf("%w: task %s depends on unknown task %s", ErrUnknownDependency, task.ID, dep)
			}
		}
	}
	if cycle, ok := findDependencyCycle(g.Tasks); ok {
		return fmt.Errorf("%w: %s", ErrDependencyCycle, strings.Join(cycle, " -> "))
	}
	return g.Budgets.validate()
}

// dependencyCycleColor marks a task's DFS visitation state while searching
// for a dependency cycle.
type dependencyCycleColor int

const (
	white dependencyCycleColor = iota // not yet visited
	gray                              // on the current DFS path
	black                             // fully explored, no cycle through it
)

// findDependencyCycle returns the first dependency cycle found, as an
// ordered list of task IDs starting and ending on the repeated task, or
// (nil, false) if the graph is acyclic. Callers must have already checked
// that every DependsOn reference resolves to a known task.
func findDependencyCycle(tasks []Task) ([]string, bool) {
	byID := make(map[string]Task, len(tasks))
	color := make(map[string]dependencyCycleColor, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
		color[t.ID] = white
	}

	var path []string
	var visit func(id string) ([]string, bool)
	visit = func(id string) ([]string, bool) {
		color[id] = gray
		path = append(path, id)
		for _, dep := range byID[id].DependsOn {
			switch color[dep] {
			case gray:
				start := 0
				for i, seen := range path {
					if seen == dep {
						start = i
						break
					}
				}
				cycle := append([]string(nil), path[start:]...)
				return append(cycle, dep), true
			case white:
				if cycle, found := visit(dep); found {
					return cycle, true
				}
			}
		}
		color[id] = black
		path = path[:len(path)-1]
		return nil, false
	}

	for _, t := range tasks {
		if color[t.ID] == white {
			if cycle, found := visit(t.ID); found {
				return cycle, true
			}
		}
	}
	return nil, false
}

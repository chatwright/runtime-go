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
// CampaignState (package actor) is a later slice.
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
	"fmt"
	"strings"
)

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

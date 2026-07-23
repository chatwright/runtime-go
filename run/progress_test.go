package run_test

import (
	"context"
	"testing"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/goal"
	"chatwright.dev/runtime/platform"
	"chatwright.dev/runtime/run"
)

// TestRunProgressReportsPartBoundaries is campaign-progress-reporting's
// run/-level MVP proof: a two-Part Run (both ai-goal, so each contributes
// its own Loop's progress too) emits run.ProgressSnapshots that mark every
// Part boundary (PartProgressStarted/PartProgressCompleted, with the right
// PartIndex/PartCount) around each Part's own forwarded task progress
// (PartProgressTask, carrying that Part's own actor.ProgressSnapshot).
func TestRunProgressReportsPartBoundaries(t *testing.T) {
	const chatID = int64(1)
	user := platform.User{ID: 9, FirstName: "Explorer"}
	clock := newFakeClock()
	emu := newFakeEmulator(clock.now)
	emu.queueBotReply(chatID, "ack one", nil)
	emu.queueBotReply(chatID, "ack two", nil)

	partAGoal := goal.Goal{ID: "a", Tasks: []goal.Task{{ID: "only"}}}
	partAProvider := actor.NewScriptedProvider(actor.Usage{Model: "scripted-v1"},
		actor.Proposal{Kind: actor.ProposeSendText, Text: "go-a"},
		actor.Proposal{Kind: actor.ProposeTaskDone},
	)
	partA := run.NewAIGoalPart("part-a", "First part", "", run.AIGoalPartInput{
		ActorID: "explorer", Goal: partAGoal, Provider: partAProvider,
		Config: actor.Config{ChatID: chatID, User: user},
	})

	partBGoal := goal.Goal{ID: "b", Tasks: []goal.Task{{ID: "only"}}}
	partBProvider := actor.NewScriptedProvider(actor.Usage{Model: "scripted-v1"},
		actor.Proposal{Kind: actor.ProposeSendText, Text: "go-b"},
		actor.Proposal{Kind: actor.ProposeTaskDone},
	)
	partB := run.NewAIGoalPart("part-b", "Second part", "", run.AIGoalPartInput{
		ActorID: "explorer", Goal: partBGoal, Provider: partBProvider,
		Config: actor.Config{ChatID: chatID, User: user},
	})

	var snapshots []run.ProgressSnapshot
	r := run.Run{
		ID:          "two-part-progress",
		Environment: run.Environment{Emulator: emu, ChatIDs: []int64{chatID}, Now: clock.now},
		Parts:       []run.Part{partA, partB},
		OnProgress:  func(s run.ProgressSnapshot) { snapshots = append(snapshots, s) },
	}

	if _, err := r.Execute(context.Background()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if len(snapshots) == 0 {
		t.Fatal("no progress snapshots emitted")
	}

	// The very first and very last snapshots are this Run's own part
	// boundaries: Part 1 starting, Part 2 completing.
	first, last := snapshots[0], snapshots[len(snapshots)-1]
	if first.Phase != run.PartProgressStarted || first.PartID != "part-a" || first.PartIndex != 1 || first.PartCount != 2 {
		t.Fatalf("first snapshot = %+v, want PartProgressStarted for part-a (1/2)", first)
	}
	if last.Phase != run.PartProgressCompleted || last.PartID != "part-b" || last.PartIndex != 2 || last.PartCount != 2 {
		t.Fatalf("last snapshot = %+v, want PartProgressCompleted for part-b (2/2)", last)
	}

	// Every PartProgressStarted/PartProgressCompleted boundary is present
	// for both parts, in order, and every PartProgressTask snapshot in
	// between carries the owning Part's own position plus its Loop's own
	// forwarded actor.ProgressSnapshot.
	var boundaries []run.PartPhase
	for _, s := range snapshots {
		switch s.Phase {
		case run.PartProgressStarted, run.PartProgressCompleted:
			boundaries = append(boundaries, s.Phase)
		case run.PartProgressTask:
			if s.Task == nil {
				t.Fatalf("PartProgressTask snapshot %+v has a nil Task", s)
			}
			if s.PartID == "part-a" && s.PartIndex != 1 {
				t.Fatalf("part-a task snapshot PartIndex = %d, want 1: %+v", s.PartIndex, s)
			}
			if s.PartID == "part-b" && s.PartIndex != 2 {
				t.Fatalf("part-b task snapshot PartIndex = %d, want 2: %+v", s.PartIndex, s)
			}
			if s.PartCount != 2 {
				t.Fatalf("task snapshot PartCount = %d, want 2: %+v", s.PartCount, s)
			}
		default:
			t.Fatalf("unexpected phase %v in snapshot %+v", s.Phase, s)
		}
	}
	wantBoundaries := []run.PartPhase{
		run.PartProgressStarted, run.PartProgressCompleted, // part-a
		run.PartProgressStarted, run.PartProgressCompleted, // part-b
	}
	if len(boundaries) != len(wantBoundaries) {
		t.Fatalf("boundary phases = %v, want %v", boundaries, wantBoundaries)
	}
	for i, want := range wantBoundaries {
		if boundaries[i] != want {
			t.Fatalf("boundary %d = %v, want %v (full sequence: %v)", i, boundaries[i], want, boundaries)
		}
	}
}

// TestRunProgressCallbackIsOptional proves a nil Run.OnProgress (the zero
// value) is a true no-op, matching every other run_test.go test that never
// sets it.
func TestRunProgressCallbackIsOptional(t *testing.T) {
	const chatID = int64(1)
	user := platform.User{ID: 9, FirstName: "Explorer"}
	clock := newFakeClock()
	emu := newFakeEmulator(clock.now)
	emu.queueBotReply(chatID, "ack", nil)

	g := goal.Goal{ID: "a", Tasks: []goal.Task{{ID: "only"}}}
	provider := actor.NewScriptedProvider(actor.Usage{Model: "scripted-v1"},
		actor.Proposal{Kind: actor.ProposeSendText, Text: "go"},
		actor.Proposal{Kind: actor.ProposeTaskDone},
	)
	part := run.NewAIGoalPart("part-a", "First part", "", run.AIGoalPartInput{
		ActorID: "explorer", Goal: g, Provider: provider,
		Config: actor.Config{ChatID: chatID, User: user},
	})

	r := run.Run{
		ID:          "no-progress-callback",
		Environment: run.Environment{Emulator: emu, ChatIDs: []int64{chatID}, Now: clock.now},
		Parts:       []run.Part{part},
	}
	if _, err := r.Execute(context.Background()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
}

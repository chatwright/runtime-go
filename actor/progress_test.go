package actor_test

import (
	"context"
	"testing"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/goal"
	"chatwright.dev/runtime/observe"
)

// TestProgressSnapshotSequenceForScriptedTwoTaskCampaign is
// campaign-progress-reporting's MVP proof: a scripted, two-task campaign
// driven by an injected fake clock emits a deterministic
// actor.ProgressSnapshot sequence — task-started, one per-iteration
// snapshot per recorded LoopEvent, task-ended, for each task in turn —
// with TaskIndex/TaskCount/TasksCompleted and budget-burn fractions
// derived correctly at every step.
func TestProgressSnapshotSequenceForScriptedTwoTaskCampaign(t *testing.T) {
	fc := newFakeChat()
	fc.seedBotMessage("Hello", nil)
	fc.queueBotReply("ack one", nil)
	fc.queueBotReply("ack two", nil)

	engine := observe.NewEngine(fc, observe.ChatRef{ChatID: testChatID})
	clock := newFakeClock()
	g := goal.Goal{
		ID: "g",
		Tasks: []goal.Task{
			{ID: "first"},
			{ID: "second"},
		},
		Budgets: goal.Budgets{MaxSteps: 10},
	}
	campaignState := mustCampaign(t, g, clock.now)

	provider := actor.NewScriptedProvider(actor.Usage{Model: "scripted"},
		actor.Proposal{Kind: actor.ProposeSendText, Text: "go"},
		actor.Proposal{Kind: actor.ProposeTaskDone},
		actor.Proposal{Kind: actor.ProposeSendText, Text: "go again"},
		actor.Proposal{Kind: actor.ProposeTaskDone},
	)

	var snapshots []actor.ProgressSnapshot
	lp := mustLoop(t, provider, engine, fc, campaignState, g, actor.Config{
		ChatID: testChatID, User: testUser, Now: clock.now,
		OnProgress: func(s actor.ProgressSnapshot) { snapshots = append(snapshots, s) },
	})

	if _, err := lp.RunCampaign(context.Background()); err != nil {
		t.Fatalf("RunCampaign() error = %v", err)
	}

	wantPhases := []actor.ProgressPhase{
		actor.ProgressTaskStarted, actor.ProgressIteration, actor.ProgressIteration, actor.ProgressTaskEnded,
		actor.ProgressTaskStarted, actor.ProgressIteration, actor.ProgressIteration, actor.ProgressTaskEnded,
	}
	if len(snapshots) != len(wantPhases) {
		t.Fatalf("emitted %d snapshots, want %d: %+v", len(snapshots), len(wantPhases), snapshots)
	}
	for i, want := range wantPhases {
		if snapshots[i].Phase != want {
			t.Fatalf("snapshot %d Phase = %v, want %v", i, snapshots[i].Phase, want)
		}
	}

	// The first task's snapshots: TaskIndex 1/2 throughout. TasksCompleted
	// is 0 until the task-done proposal is processed (campaign.Complete is
	// called INSIDE actOn, so it has already happened by the very
	// iteration snapshot for that same action, index 2), then 1.
	for i := 0; i < 4; i++ {
		s := snapshots[i]
		if s.TaskID != "first" || s.TaskIndex != 1 || s.TaskCount != 2 {
			t.Fatalf("snapshot %d = %+v, want TaskID=first TaskIndex=1 TaskCount=2", i, s)
		}
	}
	wantFirstTasksCompleted := []int{0, 0, 1, 1}
	for i, want := range wantFirstTasksCompleted {
		if snapshots[i].TasksCompleted != want {
			t.Fatalf("snapshot %d TasksCompleted = %d, want %d", i, snapshots[i].TasksCompleted, want)
		}
	}

	// The second task's snapshots: TaskIndex 2/2 throughout; TasksCompleted
	// starts at 1 (the first task) and becomes 2 once "second"'s own
	// task-done proposal is processed (index 6, same reasoning as above).
	for i := 4; i < 8; i++ {
		s := snapshots[i]
		if s.TaskID != "second" || s.TaskIndex != 2 || s.TaskCount != 2 {
			t.Fatalf("snapshot %d = %+v, want TaskID=second TaskIndex=2 TaskCount=2", i, s)
		}
	}
	wantSecondTasksCompleted := []int{1, 1, 2, 2}
	for i, want := range wantSecondTasksCompleted {
		if snapshots[4+i].TasksCompleted != want {
			t.Fatalf("snapshot %d TasksCompleted = %d, want %d", 4+i, snapshots[4+i].TasksCompleted, want)
		}
	}

	// Iteration counts increment 0 (task-started) -> 1 -> 2 -> 2 (task-ended,
	// same as the last iteration).
	wantIterations := []int{0, 1, 2, 2}
	for i, want := range wantIterations {
		if snapshots[i].Iteration != want {
			t.Fatalf("snapshot %d Iteration = %d, want %d", i, snapshots[i].Iteration, want)
		}
	}

	// Budget burn: MaxSteps=10, campaign-wide steps accumulate across both
	// tasks — by the second task's first iteration snapshot, 3 steps have
	// already been recorded (2 from "first" + 1 from "second").
	thirdIterSnap := snapshots[5] // "second"'s first ProgressIteration
	wantBurn := 3.0 / 10.0
	if thirdIterSnap.Burn.Steps != wantBurn {
		t.Fatalf("snapshot 5 Burn.Steps = %v, want %v (3 steps recorded / MaxSteps 10)", thirdIterSnap.Burn.Steps, wantBurn)
	}

	if !snapshots[7].Stopped {
		t.Fatal("final snapshot Stopped = false, want true (goal-complete)")
	}
	if snapshots[7].StopReason != goal.StopGoalComplete {
		t.Fatalf("final snapshot StopReason = %v, want StopGoalComplete", snapshots[7].StopReason)
	}
}

// TestProgressCallbackIsOptional proves a nil Config.OnProgress (the zero
// value) is a true no-op: RunTask behaves identically to every other test
// in this package that never sets it.
func TestProgressCallbackIsOptional(t *testing.T) {
	fc := newFakeChat()
	fc.seedBotMessage("Hello", nil)
	fc.queueBotReply("ack", nil)

	engine := observe.NewEngine(fc, observe.ChatRef{ChatID: testChatID})
	clock := newFakeClock()
	g := goal.Goal{ID: "g", Tasks: []goal.Task{{ID: "t1"}}}
	campaignState := mustCampaign(t, g, clock.now)
	provider := actor.NewScriptedProvider(actor.Usage{},
		actor.Proposal{Kind: actor.ProposeSendText, Text: "go"},
		actor.Proposal{Kind: actor.ProposeTaskDone},
	)
	lp := mustLoop(t, provider, engine, fc, campaignState, g, actor.Config{ChatID: testChatID, User: testUser, Now: clock.now})

	result, err := lp.RunTask(context.Background(), "t1")
	if err != nil {
		t.Fatalf("RunTask() error = %v", err)
	}
	if result.Status != goal.TaskCompleted {
		t.Fatalf("result.Status = %v, want TaskCompleted", result.Status)
	}
}

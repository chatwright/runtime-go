package run

import "chatwright.dev/runtime/actor"

// PartPhase names when a run.ProgressSnapshot was emitted relative to its
// Part — see Run.OnProgress.
type PartPhase string

// Part progress phases. See PartPhase.
const (
	// PartProgressStarted: Execute is about to run the named Part —
	// emitted for every Part kind, including deterministic ones (which
	// carry no Task).
	PartProgressStarted PartPhase = "part-started"
	// PartProgressTask: an ai-goal Part's own actor.Loop just emitted an
	// actor.ProgressSnapshot (Task is set) — forwarded here with this
	// Part's own position added.
	PartProgressTask PartPhase = "part-task"
	// PartProgressCompleted: Execute just finished running the named Part
	// (any PartStatus) — emitted for every Part kind.
	PartProgressCompleted PartPhase = "part-completed"
)

// String renders p for diagnostics and formatted stage lines.
func (p PartPhase) String() string { return string(p) }

// ProgressSnapshot wraps an ai-goal Part's own actor.ProgressSnapshot with
// this Run's own part position — the idea's "part k/n" gauge
// (spec/ideas/campaign-progress-reporting.md) — never added to a run
// bundle: derived, in-process reporting only, exactly like
// actor.ProgressSnapshot itself. See Run.OnProgress.
type ProgressSnapshot struct {
	PartID    string    `json:"partId"`
	PartIndex int       `json:"partIndex"` // 1-based
	PartCount int       `json:"partCount"`
	Phase     PartPhase `json:"phase"`

	// Task is set exactly when Phase is PartProgressTask — the forwarded
	// actor.ProgressSnapshot from that ai-goal Part's own Loop. Nil for
	// PartProgressStarted/PartProgressCompleted, and for a deterministic
	// Part's own boundary snapshots (it has no Loop to report from).
	Task *actor.ProgressSnapshot `json:"task,omitempty"`
}

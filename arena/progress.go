package arena

import (
	"fmt"

	"chatwright.dev/runtime/run"
)

// formatProgressLine renders one run.ProgressSnapshot as a single, human-
// readable stage line — spec/ideas/campaign-progress-reporting.md's own
// example: "model 2/4 · repeat 1/3 · task 1/2 · steps 5/12". pos supplies
// this cell's matrix coordinates (model/repeat, RunOptions.ProgressWriter's
// per-cell context); snap supplies the rest (part position, and — when its
// own Task is set, see run.ProgressSnapshot — task position and budget
// burn).
func formatProgressLine(spec ProviderSpec, pos matrixPosition, snap run.ProgressSnapshot) string {
	line := fmt.Sprintf("model %d/%d (%s) · repeat %d/%d · part %d/%d [%s]",
		pos.modelIndex, pos.modelCount, spec.label(),
		pos.repeat, pos.repeatCount,
		snap.PartIndex, snap.PartCount, snap.Phase)

	if t := snap.Task; t != nil {
		line += fmt.Sprintf(" · task %d/%d · step %d · steps-burn %.0f%% · non-progress %d",
			t.TaskIndex, t.TaskCount, t.Iteration, t.Burn.Steps*100, t.NonProgressStreak)
	}
	return line
}
